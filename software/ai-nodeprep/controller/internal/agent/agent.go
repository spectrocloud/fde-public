package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"spectrocloud.com/nodeprep/api/v1alpha1"
	"spectrocloud.com/nodeprep/internal/k8sutil"
)

var nodePrepsGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "nodepreps"}

const maxStepAttempts = 5

// Agent is the node-side engine. It is stateless across restarts: all
// progress lives in the NodePrep status (design §2 principle 1).
type Agent struct {
	client        kubernetes.Interface
	dyn           dynamic.Interface
	nodeName      string
	ns            string
	interval      time.Duration
	allowReboot   bool
	hostMutations bool
	rebootCommand string

	// Host filesystem locations (mounted via hostPath in the DaemonSet).
	hostKubeletDir string
	hostEtcUdev    string
	hostRunDir     string
	bfbDir         func() string
	spcxDir        func() string
	hostEtcDir     func() string

	// mellanoxFns is the Mellanox PCI inventory from the latest scan.
	mellanoxFns []pciDevice

	// mftCache holds per-PCI-address MFT classification results; failures
	// are not cached so a classification that ran before MFT was installed
	// corrects itself on a later refresh.
	mftCache map[string]mftInfo

	// mftPendingLogged fires the one-line "MFT not installed yet" inventory
	// notice once per process instead of per device per cycle (found noisy
	// live on a fresh node: 4 lines × every poll cycle through the whole
	// Provisioning window before aptPackages installs MFT).
	mftPendingLogged bool

	// rebootIssued guards the one-shot reboot execution per boot.
	rebootIssued bool

	// pendingReboot accumulates reboot requests raised by step bodies
	// during a stage pass; checkpointReboot fires them once the walk goes
	// quiet (bash needs_reboot semantics, design §5.2).
	pendingReboot []rebootRequest

	// stageProgressed reports whether the last runStage pass changed any
	// step's state or attempts — the quiescence signal for the checkpoint.
	stageProgressed bool

	// cpRole caches the node's control-plane role label for ~1min; the §6.4
	// admission gate fails closed on it (nodeIsControlPlane).
	cpRole      bool
	cpRoleKnown bool
	cpRoleUntil time.Time

	// krel caches the host's uname -r for package-name expansion
	// ($(uname -r) in firmware.doca.packages); fetched once per process.
	krel      string
	krelKnown bool

	// noPrepLogged keeps the "no NodePrep yet" line to one occurrence
	// instead of one per poll cycle.
	noPrepLogged bool

	// backoffUntil paces re-running steps that just Failed (design §5.1
	// retry budget): in-memory, doubling with each consecutive failure,
	// cleared on success and on the resume annotation.
	backoffUntil map[string]time.Time

	// fwResetChips dedups the mlxfwreset firmware reset (0.1.59) to one run
	// per physical chip per step pass — the mezz's two PFs share one
	// firmware, so a reset staged for either function reinitializes both,
	// and a second reset would just bounce the driver again. Cleared at the
	// top of the steps that issue resets.
	fwResetChips map[string]bool

	// sriovRewindRequested is pass-scoped (reset at the top of runStage):
	// stepSriovNumVFs sets it when an imported Finalizing walk blocks on a
	// VF demand its Configuring stage never ran (0.1.60), and
	// maybeRewindImportedWalk consumes it after the step loop — the rewind
	// must ride runStage's own ledger patch, since a patch from inside the
	// step body would be clobbered by runStage's full-ledger write.
	sriovRewindRequested bool

	// lastBootVerify paces Ready-phase boot-verify: verify runs immediately
	// on a boot change, otherwise at most every bootVerifyInterval (the
	// critical-step bodies cost hundreds of host commands per pass).
	lastBootVerify struct {
		bootID string
		at     time.Time
	}

	// hookDone remembers the boot-hook content already verified/enabled this
	// process lifetime, so the steady-state cycle costs zero host execs.
	hookDone string

	// verbose (-verbose / NODEPREP_VERBOSE=true, troubleshooting) logs every
	// host exec in full: quiet sweeps and tool dumps (mlxconfig/flint
	// queries, the ACS lspci+setpci traffic) become visible again.
	verbose bool
}

// bootVerifyInterval paces the Ready-phase re-verification. A new boot
// always verifies immediately (design §6.1); between boots the drift check
// runs on this maintenance cadence, not every poll cycle.
const bootVerifyInterval = 5 * time.Minute

// maxLoadReboots is gone (0.1.51): the sriovNumVFs load-reboot attempt is
// now tracked in the sriov-nv-stage NodePrep annotation — durable across
// agent restarts and node reboots, unlike the in-memory counter 0.1.50
// used, which counted poll passes instead of reboots and reset on every
// boot (bl-r1-c2-06 reboot-cycled live).

func New(client kubernetes.Interface, dyn dynamic.Interface, nodeName, ns string, interval time.Duration, allowReboot, hostMutations, verbose bool, rebootCommand string) *Agent {
	return &Agent{
		client: client, dyn: dyn, nodeName: nodeName, ns: ns,
		interval: interval, allowReboot: allowReboot, hostMutations: hostMutations,
		verbose:        verbose,
		rebootCommand:  rebootCommand,
		backoffUntil:   map[string]time.Time{},
		mftCache:       map[string]mftInfo{},
		hostKubeletDir: "/host/var/lib/kubelet",
		hostEtcUdev:    "/host/etc/udev/rules.d",
		hostRunDir:     "/host/run",
		bfbDir:         func() string { return "/host/opt/spectrocloud/spcx/bfb" },
		spcxDir:        func() string { return "/host/opt/spectrocloud/spcx" },
		hostEtcDir:     func() string { return "/host/etc" },
	}
}

// Run drives the poll loop. Polling (rather than watches) keeps v0.1 small;
// the design's watch-driven engine replaces it in v0.2.
func (a *Agent) Run(ctx context.Context) error {
	bootID, err := readBootID()
	if err != nil {
		return fmt.Errorf("read boot_id: %w", err)
	}
	a.logf("starting on node %s, boot %s", a.nodeName, bootID)

	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		a.cycle(ctx, bootID)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func readBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (a *Agent) fetchNodePrep(ctx context.Context) (*v1alpha1.NodePrep, error) {
	u, err := a.dyn.Resource(nodePrepsGVR).Get(ctx, a.nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(u.Object)
	if err != nil {
		return nil, err
	}
	np := &v1alpha1.NodePrep{}
	if err := json.Unmarshal(raw, np); err != nil {
		return nil, err
	}
	return np, nil
}

func (a *Agent) fetchProfile(ctx context.Context, np *v1alpha1.NodePrep) (*v1alpha1.NodePrepProfile, error) {
	profilesGVR := schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "nodeprepprofiles"}
	u, err := a.dyn.Resource(profilesGVR).Get(ctx, np.Spec.ProfileRef.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(u.Object)
	if err != nil {
		return nil, err
	}
	p := &v1alpha1.NodePrepProfile{}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, err
	}
	return p, nil
}

// logf prints the agent's log line. Every host action, step result, and
// phase transition flows through here (design §2: observable by default),
// so pod logs alone tell an operator what the agent did and why.
func (a *Agent) logf(format string, args ...interface{}) {
	fmt.Printf("[nodeprep-agent] "+format+"\n", args...)
}

func (a *Agent) emit(ctx context.Context, eventType, reason, message string) {
	a.logf("event %s %s: %s", eventType, reason, message)
	k8sutil.Emit(ctx, a.client, v1alpha1.NodePrepKind, a.nodeName, eventType, reason, message)
}

// patchStatus merges a partial status onto the NodePrep status subresource.
func (a *Agent) patchStatus(ctx context.Context, partial map[string]interface{}) error {
	patch, err := json.Marshal(map[string]interface{}{"status": partial})
	if err != nil {
		return err
	}
	_, err = a.dyn.Resource(nodePrepsGVR).Patch(ctx, a.nodeName, types.MergePatchType, patch, metav1.PatchOptions{}, "status")
	return err
}

func (a *Agent) setPhase(ctx context.Context, phase v1alpha1.Phase, reason, message string) error {
	if err := a.patchStatus(ctx, map[string]interface{}{"phase": string(phase)}); err != nil {
		a.logf("phase patch to %s failed: %v", phase, err)
		return err
	}
	a.emit(ctx, corev1.EventTypeNormal, "PhaseTransition", fmt.Sprintf("phase -> %s (%s): %s", phase, reason, message))
	return nil
}

// syncColdPhase mirrors the SR-IOV cold halt into .status.phase (0.1.54):
// while the ColdRebootRequired condition is True mid-walk in Finalizing,
// the phase reads ColdRebootRequired so an operator sees at a glance that
// the node waits for a manual power cycle; when the condition clears the
// phase resumes the Finalizing walk it parked. Both directions only fire
// on an actual mismatch, so the 5s poll never churns the phase. A Ready
// node (boot-verify re-runs the sriov step body) and a Failed node keep
// their phase — the condition alone names the situation there, and
// resuming from the ledger stage would wrongly demote Ready.
func (a *Agent) syncColdPhase(ctx context.Context, np *v1alpha1.NodePrep) {
	cold := k8sutil.ConditionStatus(np.Status.Conditions, v1alpha1.ConditionColdRebootRequired) == "True"
	switch {
	case cold && np.Status.Phase == v1alpha1.PhaseFinalizing:
		if err := a.setPhase(ctx, v1alpha1.PhaseColdRebootRequired, "cold reboot required",
			"firmware NV changes need a manual power cycle (power off, then on); the walk parks here until then"); err == nil {
			np.Status.Phase = v1alpha1.PhaseColdRebootRequired
		}
	case !cold && np.Status.Phase == v1alpha1.PhaseColdRebootRequired:
		if err := a.setPhase(ctx, v1alpha1.PhaseFinalizing, "cold halt resolved",
			"the ColdRebootRequired condition cleared; the parked Finalizing walk resumes"); err == nil {
			np.Status.Phase = v1alpha1.PhaseFinalizing
		}
	}
}

// cycle is one reconciliation pass.
func (a *Agent) cycle(ctx context.Context, bootID string) {
	np, err := a.fetchNodePrep(ctx)
	if err != nil {
		// Almost always "not adopted yet": the controller creates the
		// NodePrep only when a profile's nodeSelector matches this node.
		// Log once so an idle agent is explainable from its logs alone.
		if !a.noPrepLogged {
			a.noPrepLogged = true
			a.logf("no NodePrep object for node %s yet (%v); waiting for the controller to adopt it", a.nodeName, err)
		}
		return
	}
	a.noPrepLogged = false

	// Status is ignored on CREATE for CRDs with a status subresource, so a
	// NodePrep created before the controller wrote the initial phase (or one
	// created by hand) arrives with an empty phase. Pending is the canonical
	// start (design §5.2); heal it instead of idling in UnknownPhase.
	if np.Status.Phase == "" {
		np.Status.Phase = v1alpha1.PhasePending
		if err := a.patchStatus(ctx, map[string]interface{}{"phase": string(v1alpha1.PhasePending)}); err != nil {
			a.logf("initial phase patch failed: %v", err)
			return
		}
	}
	profile, err := a.fetchProfile(ctx, np)
	if err != nil {
		a.emit(ctx, corev1.EventTypeWarning, "ProfileMissing", fmt.Sprintf("profile %s unavailable: %v", np.Spec.ProfileRef.Name, err))
		return
	}

	// Profile edit (design §5.1, the coarse v0.1 form of inputsHash): a new
	// profile generation invalidates every step — the machine re-walks from
	// Provisioning and each step re-converges. All steps are idempotent
	// (detect first, apply only what's missing), so a no-op edit just
	// re-verifies. The observed generation is advanced here so the re-open
	// happens exactly once per edit; per-step input hashing lands with real
	// Apply.
	if profile.Generation != np.Status.ObservedProfileGeneration {
		a.logf("profile %s generation %d -> %d; re-opening all steps",
			np.Spec.ProfileRef.Name, np.Status.ObservedProfileGeneration, profile.Generation)
		steps := np.Status.Steps
		for i := range steps {
			if steps[i].State == v1alpha1.StepPending {
				continue
			}
			steps[i] = v1alpha1.StepStatus{Name: steps[i].Name, Stage: steps[i].Stage, State: v1alpha1.StepPending}
		}
		a.pendingReboot = nil // the re-walk raises fresh requests as it converges
		if err := a.patchStatus(ctx, map[string]interface{}{
			"steps":                     steps,
			"observedProfileGeneration": profile.Generation,
		}); err != nil {
			a.logf("profile-change reset failed: %v", err)
			return
		}
		np.Status.Steps = steps
		np.Status.ObservedProfileGeneration = profile.Generation
		a.emit(ctx, corev1.EventTypeNormal, "ProfileChanged",
			fmt.Sprintf("profile %s generation %d; all steps re-opened", np.Spec.ProfileRef.Name, profile.Generation))
		a.setPhase(ctx, v1alpha1.PhaseProvisioning, "profile changed",
			fmt.Sprintf("profile generation %d applied; steps re-opened", profile.Generation))
		return
	}

	// Boot protocol (design §5.2): detect boot_id changes, clear the
	// RebootRequired condition, count the reboot, and re-verify at boot.
	changedBoot := np.Status.BootID != "" && np.Status.BootID != bootID
	if changedBoot || np.Status.BootID == "" {
		perStage := np.Status.Reboots.PerStage
		if perStage == nil {
			perStage = map[string]int{}
		}
		total := np.Status.Reboots.Total
		phase := np.Status.Phase
		if changedBoot {
			total++
			perStage[string(phase)]++
		}
		steps := np.Status.Steps
		conds := np.Status.Conditions
		if changedBoot {
			k8sutil.SetCondition(&conds, v1alpha1.ConditionBootVerified, "False", v1alpha1.ReasonPending,
				"new boot detected; runtime-critical steps must re-verify before the taint is released", 0)
			k8sutil.SetCondition(&conds, v1alpha1.ConditionRebootRequired, "False", v1alpha1.ReasonVerified,
				"reboot completed (boot_id changed)", 0)
			// Runtime-transient steps re-verify at every boot (bash
			// complete-stage semantics: the OS-level state is (re)established
			// after the last reboot, not before it). A nodeprep-requested
			// reboot after these steps ran — e.g. a Configuring mlxconfig
			// reboot riding into the Finalizing stage — wipes the OS-level
			// state: sriov_numvfs resets to 0 on the driver re-probe, the
			// per-VF sriov/ sysfs dirs vanish with it, setpci's ACS clears
			// are lost, and the VF netdevs' MTU/link state (ip link) resets.
			// Done steps never re-run, so without this the walk parks forever
			// (found live on bl-r1-c2-02: vfGuids blocked on absent sriov/0
			// while sriovNumVFs read Done "0→1" against a host showing
			// numvfs=0). All four are cheap, detect-first and idempotent:
			// converged hosts re-verify in one pass.
			for i := range steps {
				switch steps[i].Name {
				case "sriovNumVFs", "vfGuids", "disableACS", "udevRules":
					if steps[i].State == v1alpha1.StepDone {
						steps[i] = v1alpha1.StepStatus{Name: steps[i].Name, Stage: steps[i].Stage, State: v1alpha1.StepPending}
					}
				}
			}
		}
		if err := a.patchStatus(ctx, map[string]interface{}{
			"bootId": bootID,
			"reboots": map[string]interface{}{
				"total":    total,
				"perStage": perStage,
			},
			"conditions": conds,
			"steps":      steps,
		}); err != nil {
			a.logf("boot sync failed: %v", err)
			return
		}
		// Mirror what was patched back into the in-memory np: runStage below
		// reads np.Status.Reboots.Total, and the cycle that detects the boot
		// must already see the incremented count. Without the mirror the
		// first post-boot pass re-derives the pre-boot stage outcome — a
		// staged SR-IOV load re-requested its warm reboot from a stale
		// reboots=0, and the stale request fired a second reboot from the
		// next quiet checkpoint (live on bl-r1-c2-06, 0.1.51).
		np.Status.Reboots.Total = total
		np.Status.Reboots.PerStage = perStage
		if changedBoot {
			a.emit(ctx, corev1.EventTypeNormal, "BootDetected", fmt.Sprintf("boot changed %s -> %s; reboot #%d recorded", np.Status.BootID, bootID, total))
			np.Status.BootID = bootID
			np.Status.Conditions = conds
		}
	}

	// The cold halt shows in the phase at a glance (0.1.54); see
	// syncColdPhase. Runs before every early return so the phase stays
	// truthful even while a requested reboot holds the walk.
	a.syncColdPhase(ctx, np)

	// Resume annotation on a Failed NodePrep (design §5.2 recovery).
	if np.Status.Phase == v1alpha1.PhaseFailed {
		if _, ok := np.Annotations[v1alpha1.ResumeAnnotation]; ok {
			if err := a.removeAnnotation(ctx, v1alpha1.ResumeAnnotation); err == nil {
				a.backoffUntil = map[string]time.Time{}
				steps := np.Status.Steps
				for i := range steps {
					if steps[i].State == v1alpha1.StepFailed {
						steps[i] = v1alpha1.StepStatus{Name: steps[i].Name, Stage: steps[i].Stage, State: v1alpha1.StepPending}
					}
				}
				_ = a.patchStatus(ctx, map[string]interface{}{"steps": steps})
				a.setPhase(ctx, v1alpha1.PhaseProvisioning, "resume", "resume annotation processed; steps reset")
				a.emit(ctx, corev1.EventTypeNormal, "Resumed", "NodePrep resumed by operator annotation")
				return
			}
		}
	}

	// Refresh inventory each cycle (cheap, sysfs-only in v0.1).
	if err := a.refreshInventory(ctx, np, profile); err != nil {
		a.logf("inventory scan failed: %v", err)
	}

	// Install the boot hook before any stage can request a reboot (design
	// §6.2): the hook carries the guarded kubelet manager-state reset into
	// every future boot, and machines with stale memory-manager state
	// crashloop kubelet on a reboot that happens before the Finalizing
	// kubeletState step would have installed it. Mutations-gated like every
	// host write; detect-only runs keep the status quo.
	if a.mutationsAllowed(profile) {
		if msg, err := a.ensureBootHook(profile); err != nil {
			a.logf("boot hook install failed: %v", err)
		} else if msg != "" {
			a.logf("boot hook: %s", msg)
		}
	}

	// A requested reboot is in flight (design §5.2 config→reboot→precomplete):
	// hold the walk. The host is about to go down — running further steps
	// (kubelet restarts, driver markers) on a dying host is wasted work and
	// the ledger reads wrong afterwards. The boot_id change clears the
	// condition above and re-verification resumes the walk. Without
	// -allow-reboot there is nothing to wait for: keep walking so the
	// blocked steps stay visible.
	if a.allowReboot && k8sutil.ConditionStatus(np.Status.Conditions, v1alpha1.ConditionRebootRequired) == "True" {
		a.logf("reboot requested and pending; holding the walk until the boot_id changes")
		return
	}

	// Run the current stage. The cold overlay phase (0.1.54) belongs here
	// too: parking the phase must not stop the walk, because the Finalizing
	// step bodies are what re-check sriov_totalvfs and observe the
	// convergence after the operator's power cycle. Without the case the
	// switch idled in UnknownPhase and the halt could never clear
	// (0.1.55; found live on bl-r1-c2-06 during its power cycle).
	switch np.Status.Phase {
	case v1alpha1.PhasePending, v1alpha1.PhaseProvisioning, v1alpha1.PhaseFlashing, v1alpha1.PhaseConfiguring, v1alpha1.PhaseFinalizing, v1alpha1.PhaseColdRebootRequired:
		a.runStage(ctx, np, profile)
		a.checkpointReboot(ctx)
	case v1alpha1.PhaseReady:
		a.pendingReboot = nil // goals met; the walk no longer owes a reboot
		a.verifyReady(ctx, np, profile, bootID)
	case v1alpha1.PhaseFailed:
		a.pendingReboot = nil // hold; recovery is the resume annotation (design §5.2)
	default:
		a.emit(ctx, corev1.EventTypeWarning, "UnknownPhase", fmt.Sprintf("unknown phase %q", np.Status.Phase))
	}
}

// nodeIsControlPlane reports whether this node carries the control-plane
// role label (design §6.4 gate). Cached ~1min; until the first successful
// read it conservatively reports control plane so the admission gate fails
// closed — a worker merely idles one cycle, a CP never preps un-admitted.
func (a *Agent) nodeIsControlPlane(ctx context.Context) bool {
	if time.Now().Before(a.cpRoleUntil) {
		return a.cpRole
	}
	node, err := a.client.CoreV1().Nodes().Get(ctx, a.nodeName, metav1.GetOptions{})
	if err != nil {
		if !a.cpRoleKnown {
			a.logf("control-plane role check failed (%v); holding CP admission gate until the node is readable", err)
			return true
		}
		return a.cpRole // stale but recent answer
	}
	_, cp := node.Labels["node-role.kubernetes.io/control-plane"]
	a.cpRole, a.cpRoleKnown, a.cpRoleUntil = cp, true, time.Now().Add(time.Minute)
	return cp
}

func (a *Agent) removeAnnotation(ctx context.Context, key string) error {
	patch, _ := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": map[string]interface{}{key: nil}},
	})
	_, err := a.dyn.Resource(nodePrepsGVR).Patch(ctx, a.nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// setAnnotation patches one metadata annotation and mirrors it into np's
// in-memory copy, so a later read of np.Annotations sees the value the API
// now holds (stepSriovNumVFs advances the sriov-nv-stage machine through
// this on state transitions only — SetCondition-style no-op semantics keep
// the 5s poll from churning resourceVersions).
func (a *Agent) setAnnotation(np *v1alpha1.NodePrep, key, value string) error {
	patch, _ := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": map[string]interface{}{key: value}},
	})
	_, err := a.dyn.Resource(nodePrepsGVR).Patch(context.Background(), a.nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return err
	}
	if np.Annotations == nil {
		np.Annotations = map[string]string{}
	}
	np.Annotations[key] = value
	return nil
}

// refreshInventory scans sysfs and writes nics/gpus to status. The patch and
// its log line fire only when the scan changed, so an idle node does not
// churn the NodePrep's resourceVersion every poll cycle.
func (a *Agent) refreshInventory(ctx context.Context, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) error {
	nics, gpus, mellanox, err := scanInventory(profile)
	if err != nil {
		return err
	}
	if len(mellanox) > 0 {
		a.enrichMellanox(mellanox)
		// Re-render from the enriched devices, PFs only (0.1.57): the
		// previous index mapping nics[i] = mellanox[i] silently depended
		// on nics and mellanox having the same length and order.
		nics = nics[:0]
		for _, d := range mellanox {
			if d.isVF {
				continue
			}
			nics = append(nics, d.nicStatus())
		}
	}
	a.mellanoxFns = mellanox
	if reflect.DeepEqual(np.Status.Nics, nics) && reflect.DeepEqual(np.Status.Gpus, gpus) {
		return nil
	}
	a.logf("inventory: %d NIC(s), %d GPU(s), %d Mellanox function(s)", len(nics), len(gpus), len(mellanox))
	np.Status.Nics = nics
	np.Status.Gpus = gpus
	return a.patchStatus(ctx, map[string]interface{}{
		"nics": nics,
		"gpus": gpus,
	})
}

// runStage executes the steps of the current phase, advances on completion,
// and enforces the flash window / CP admission gates (design §5.2, §9.1).
func (a *Agent) runStage(ctx context.Context, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) {
	phase := walkStageFor(np.Status.Phase)
	// Quiescence signal for the reboot checkpoint: only a pass that actually
	// changed a step counts as progress; early returns (admission gates)
	// leave it false so an already-applied mutation's reboot still fires.
	a.stageProgressed = false
	// Pass-scoped request from the step bodies (sriovNumVFs sets it when an
	// imported Finalizing walk blocks on a VF raise its Configuring stage
	// never ran); consumed by maybeRewindImportedWalk after the step loop.
	a.sriovRewindRequested = false
	if phase == v1alpha1.PhasePending {
		a.setPhase(ctx, v1alpha1.PhaseProvisioning, "start", "inventory done; starting provisioning")
		return
	}

	// Admission gates: flashing requires the fleet window; control-plane
	// prep requires the serial maintenance window (design §6.4).
	if phase == v1alpha1.PhaseProvisioning {
		if k8sutil.ConditionStatus(np.Status.Conditions, v1alpha1.ConditionFlashAdmitted) == "False" {
			return // controller will flip the condition; next cycle proceeds
		}
		// A missing MaintenanceAdmitted condition is NOT consent: the
		// controller sets it on its first reconcile, and prepping before
		// that would let a freshly adopted CP run in parallel with an
		// admitted peer (found live: a queued CP walked Provisioning and
		// requested a reboot while another member was mid-prep).
		if a.nodeIsControlPlane(ctx) &&
			k8sutil.ConditionStatus(np.Status.Conditions, v1alpha1.ConditionMaintenanceAdmitted) != "True" {
			return
		}
	}

	defs := stepsForStage(phase)
	ledger := ensureSteps(np.Status.Steps, defs, phase)
	np.Status.Steps = ledger

	blocked, failed, allDone := false, false, true
	for _, def := range defs {
		s := stepByName(ledger, def.name)
		if s == nil {
			continue // cannot happen: ensureSteps adds every def
		}
		if s.State == v1alpha1.StepDone {
			continue
		}
		if until, ok := a.backoffUntil[s.Name]; ok && time.Now().Before(until) {
			allDone = false // pacing a recent failure; retry on a later cycle
			continue
		}
		prev := *s
		state, msg := def.run(a, np, profile)
		s.Message = msg
		var wait time.Duration
		switch state {
		case v1alpha1.StepDone:
			s.State = v1alpha1.StepDone
			now := metav1.Now()
			s.CompletedAt = &now
			delete(a.backoffUntil, s.Name)
		case v1alpha1.StepBlocked:
			s.State = v1alpha1.StepBlocked
			blocked = true
			allDone = false
		case v1alpha1.StepFailed:
			s.Attempts++
			s.State = v1alpha1.StepFailed
			allDone = false
			// Pacing: hold the step back for a doubling interval so a
			// flaky mirror cannot burn the whole budget in one poll window
			// (in-memory only; an agent restart just resets the pacing).
			wait = a.interval << min(s.Attempts-1, 5)
			if wait > 2*time.Minute {
				wait = 2 * time.Minute
			}
			a.backoffUntil[s.Name] = time.Now().Add(wait)
			if s.Attempts >= maxStepAttempts {
				failed = true
			}
		}
		// Every step result change is logged, so pod logs alone tell an
		// operator what ran and why (design §2). Unchanged results stay
		// silent to keep the 5s poll from spamming.
		if s.State != prev.State || s.Message != prev.Message || s.Attempts != prev.Attempts {
			// Message-only churn is not progress (a blocked step re-reports
			// the same wall every cycle); state and attempt changes are.
			if s.State != prev.State || s.Attempts != prev.Attempts {
				a.stageProgressed = true
			}
			switch s.State {
			case v1alpha1.StepDone:
				a.logf("step %s done: %s", def.name, s.Message)
			case v1alpha1.StepBlocked:
				a.logf("step %s blocked: %s", def.name, s.Message)
			case v1alpha1.StepFailed:
				a.logf("step %s failed (attempt %d/%d, retry in %s): %s", def.name, s.Attempts, maxStepAttempts, wait, s.Message)
			default:
				a.logf("step %s -> %s: %s", def.name, s.State, s.Message)
			}
		}
	}
	ledger, rewound := a.maybeRewindImportedWalk(np, ledger)
	if err := a.patchStatus(ctx, map[string]interface{}{"steps": ledger}); err != nil {
		a.logf("step ledger patch failed: %v", err)
	} else {
		np.Status.Steps = ledger
		if rewound {
			// The ledger now carries the re-opened Configuring stage; flip
			// the phase so the walk actually re-enters it. The guard
			// annotation goes down only after the phase patch succeeds — a
			// failed flip must re-fire the rewind next pass, not park it.
			if err := a.setPhase(ctx, v1alpha1.PhaseConfiguring, "vf raise rewind",
				"the walk was imported at Finalizing, but the profile's VF demand exceeds the committed NV; re-running Configuring so the mlxconfig step can raise NUM_OF_VFS"); err != nil {
				a.logf("sriov-rewind phase flip failed: %v", err)
			} else {
				np.Status.Phase = v1alpha1.PhaseConfiguring
				if err := a.setAnnotation(np, sriovRewoundAnnotation, time.Now().UTC().Format(time.RFC3339)); err != nil {
					a.logf("sriov-rewound annotation write failed: %v", err)
				}
			}
		}
	}

	if failed {
		name := firstStepNamed(ledger, v1alpha1.StepFailed)
		a.emit(ctx, corev1.EventTypeWarning, "StepFailed",
			fmt.Sprintf("step %s exhausted %d attempts; NodePrep failed (resume with annotation %s)", name, maxStepAttempts, v1alpha1.ResumeAnnotation))
		a.setPhase(ctx, v1alpha1.PhaseFailed, "retry budget", "retry budget exhausted on "+name)
		return
	}
	if blocked {
		return // hold phase; the Ready condition names the blocked step
	}
	if !allDone {
		return // a step is Failed under budget; hold and retry — never advance past it
	}

	// Stage complete → advance.
	next, ok := nextStage(phase)
	if !ok {
		return
	}
	if next == v1alpha1.PhaseReady {
		a.finalizeAndVerify(ctx, np, profile)
		return
	}
	a.setPhase(ctx, next, "stage complete", fmt.Sprintf("all %s steps done", phase))
}

// finalizeAndVerify runs boot-verify before entering Ready (design §6.1).
func (a *Agent) finalizeAndVerify(ctx context.Context, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) {
	if a.bootVerify(ctx, np, profile) {
		conds := np.Status.Conditions
		k8sutil.SetCondition(&conds, v1alpha1.ConditionBootVerified, "True", v1alpha1.ReasonVerified,
			"runtime-critical steps verified on boot "+np.Status.BootID, 0)
		_ = a.patchStatus(ctx, map[string]interface{}{"conditions": conds})
		a.setPhase(ctx, v1alpha1.PhaseReady, "boot-verify", "all stages complete and boot verified; taint release follows")
		return
	}
	conds := np.Status.Conditions
	k8sutil.SetCondition(&conds, v1alpha1.ConditionBootVerified, "False", v1alpha1.ReasonPending,
		"boot verification has not passed", 0)
	_ = a.patchStatus(ctx, map[string]interface{}{"conditions": conds})
	a.emit(ctx, corev1.EventTypeWarning, "BootVerifyFailed", "runtime-critical steps are not clean; staying in Finalizing")
}

// verifyReady re-checks a Ready node: boot-verify must pass on the current
// boot before BootVerified stays True. Drift flips BootVerified to False,
// which re-applies the taint — the maintenance-window re-entry (design §6.1).
// A boot change verifies immediately; between boots the check runs at most
// every bootVerifyInterval — the critical-step bodies cost hundreds of host
// commands (mlxconfig queries, an ACS setpci sweep) per pass, and running
// them every poll cycle was continuous exec spam on the node.
func (a *Agent) verifyReady(ctx context.Context, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile, bootID string) {
	newBoot := a.lastBootVerify.bootID != bootID
	if !newBoot && time.Since(a.lastBootVerify.at) < bootVerifyInterval {
		return
	}
	verified := k8sutil.ConditionStatus(np.Status.Conditions, v1alpha1.ConditionBootVerified) == "True"
	defer func() { a.lastBootVerify.bootID, a.lastBootVerify.at = bootID, time.Now() }()
	if a.bootVerify(ctx, np, profile) {
		if !verified {
			conds := np.Status.Conditions
			k8sutil.SetCondition(&conds, v1alpha1.ConditionBootVerified, "True", v1alpha1.ReasonVerified,
				"boot verification passed on boot "+np.Status.BootID, 0)
			_ = a.patchStatus(ctx, map[string]interface{}{"conditions": conds})
			a.emit(ctx, corev1.EventTypeNormal, "BootVerified", "taint may be released")
		}
		return
	}
	// A failed pass flips the condition (and re-taints) once; the retry is
	// paced by the interval like a passing one.
	if verified {
		conds := np.Status.Conditions
		k8sutil.SetCondition(&conds, v1alpha1.ConditionBootVerified, "False", v1alpha1.ReasonDriftDetected,
			"drift detected on Ready node; taint re-applied pending repair", 0)
		_ = a.patchStatus(ctx, map[string]interface{}{"conditions": conds})
		a.emit(ctx, corev1.EventTypeWarning, "DriftDetected", "Ready node drifted; taint re-applied (design §6.1 maintenance window)")
	}
}

// bootVerify re-runs Detect over the runtime-critical steps (design §6.1).
// Fresh state and messages are written into the step ledger as it goes — a
// verify pass is the step's latest outcome while Ready, and a
// provisioning-era message would otherwise keep describing state (e.g. a
// superseded inventory classification) for the life of the boot.
func (a *Agent) bootVerify(ctx context.Context, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) bool {
	changed := false
	for _, def := range stepDefs {
		if !def.critical {
			continue
		}
		state, msg := def.run(a, np, profile)
		if s := stepByName(np.Status.Steps, def.name); s != nil {
			// A changed result is logged as well as patched: verify passes
			// run quiet (the sweeps are hostExecQuiet), so this line is the
			// only log record of, say, an ACS drift repair.
			if s.State != state || s.Message != msg {
				a.logf("boot-verify step %s: %s — %s", def.name, state, msg)
			}
			// The state lands in the ledger alongside the message: the
			// boot-transient re-open (boot change on an already-Ready
			// node) flips those steps to Pending, and a message-only
			// patch left them Pending forever even though this pass had
			// just run their bodies to Done — found live on bl-r1-c2-02
			// boot #7: a Ready, boot-verified node whose ledger showed
			// sriovNumVFs/vfGuids/udevRules/disableACS all Pending.
			if s.State != state {
				s.State = state
				changed = true
			}
			if s.Message != msg {
				s.Message = msg
				changed = true
			}
		}
		if state != v1alpha1.StepDone {
			if changed {
				_ = a.patchStatus(ctx, map[string]interface{}{"steps": np.Status.Steps})
			}
			return false
		}
	}
	if changed {
		if err := a.patchStatus(ctx, map[string]interface{}{"steps": np.Status.Steps}); err != nil {
			a.logf("boot-verify step patch failed: %v", err)
		}
	}
	return true
}

// ensureSteps merges the current stage's step definitions into the
// persistent ledger. Prior stages keep their entries — the ledger is the
// node's full prep history, not just the active stage. A definition for a
// stage the node has already walked through (first seen by a newer agent on
// a mid-prep node) is recorded Done: it ran before the ledger began
// accumulating.
func ensureSteps(existing []v1alpha1.StepStatus, defs []stepDef, phase v1alpha1.Phase) []v1alpha1.StepStatus {
	out := append(existing[:0:0], existing...)
	known := map[string]bool{}
	for _, s := range out {
		known[s.Name] = true
	}
	for _, d := range defs {
		if known[d.name] {
			continue
		}
		known[d.name] = true
		st, msg := v1alpha1.StepPending, ""
		var done *metav1.Time
		if stageRank(d.stage) < stageRank(phase) {
			st, msg = v1alpha1.StepDone, "completed before the ledger began accumulating"
			now := metav1.Now()
			done = &now
		}
		out = append(out, v1alpha1.StepStatus{Name: d.name, Stage: d.stage, State: st, Message: msg, CompletedAt: done})
	}
	return out
}

// stepByName returns a pointer into the ledger slice, so runStage can mutate
// the entry that gets patched back to the API server.
func stepByName(steps []v1alpha1.StepStatus, name string) *v1alpha1.StepStatus {
	for i := range steps {
		if steps[i].Name == name {
			return &steps[i]
		}
	}
	return nil
}

// stageRank orders the phase chain for the ledger backfill decision.
func stageRank(p v1alpha1.Phase) int {
	switch p {
	case v1alpha1.PhasePending:
		return 0
	case v1alpha1.PhaseProvisioning:
		return 1
	case v1alpha1.PhaseFlashing:
		return 2
	case v1alpha1.PhaseConfiguring:
		return 3
	case v1alpha1.PhaseFinalizing:
		return 4
	case v1alpha1.PhaseReady:
		return 5
	}
	return -1
}

func firstStepNamed(steps []v1alpha1.StepStatus, state v1alpha1.StepState) string {
	for _, s := range steps {
		if s.State == state {
			return s.Name
		}
	}
	return "unknown"
}

// walkStageFor resolves the stage whose steps a walk pass executes. The
// cold overlay phase (0.1.54) parks the Finalizing walk but must NOT stop
// it: its step bodies are what re-check sriov_totalvfs and observe the
// convergence after the operator's power cycle. Everything phase-keyed in
// runStage — step selection, ledger backfill, stage advance — goes through
// this mapping.
func walkStageFor(p v1alpha1.Phase) v1alpha1.Phase {
	if p == v1alpha1.PhaseColdRebootRequired {
		return v1alpha1.PhaseFinalizing
	}
	return p
}

func nextStage(p v1alpha1.Phase) (v1alpha1.Phase, bool) {
	switch p {
	case v1alpha1.PhaseProvisioning:
		return v1alpha1.PhaseFlashing, true
	case v1alpha1.PhaseFlashing:
		return v1alpha1.PhaseConfiguring, true
	case v1alpha1.PhaseConfiguring:
		return v1alpha1.PhaseFinalizing, true
	case v1alpha1.PhaseFinalizing:
		return v1alpha1.PhaseReady, true
	}
	return "", false
}

// patchCondition merges ONE condition into the NodePrep's CURRENT conditions
// (fresh read, modify, write). Patching a whole conditions array built from
// a stale or partial copy (found live) deletes conditions the controller
// wrote in the meantime — its MaintenanceAdmitted gate disappeared and a
// queued control plane started prepping un-admitted.
func (a *Agent) patchCondition(ctx context.Context, c metav1.Condition) {
	np, err := a.fetchNodePrep(ctx)
	if err != nil {
		a.logf("condition patch skipped: %v", err)
		return
	}
	conds := np.Status.Conditions
	k8sutil.SetCondition(&conds, c.Type, string(c.Status), c.Reason, c.Message, c.ObservedGeneration)
	if err := a.patchStatus(ctx, map[string]interface{}{"conditions": conds}); err != nil {
		a.logf("condition patch failed: %v", err)
	}
}

// requestReboot records the requirement in status and — once, per boot,
// when -allow-reboot is set — executes the reboot command after a grace
// period (design §5.2 protocol step 3). Callers reach it through the
// checkpoint (checkpointReboot), not directly from step bodies.
func (a *Agent) requestReboot(ctx context.Context, reason, message string) {
	a.patchCondition(ctx, metav1.Condition{
		Type:    v1alpha1.ConditionRebootRequired,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
	if !a.allowReboot {
		a.emit(ctx, corev1.EventTypeWarning, "RebootSuppressed", fmt.Sprintf("reboot requested (%s) but -allow-reboot is false", reason))
		return
	}
	if a.rebootIssued {
		return
	}
	a.rebootIssued = true
	a.emit(ctx, corev1.EventTypeNormal, "Rebooting", "nodeprep-initiated reboot (design §5.2)")
	go func() {
		time.Sleep(60 * time.Second) // status-write grace, as shutdown -r +1
		a.logf("executing reboot command: %s", a.rebootCommand)
		parts := strings.Fields(a.rebootCommand)
		if len(parts) == 0 {
			return
		}
		cmd := exec.Command(parts[0], parts[1:]...) // #nosec G204 -- operator-configured command
		if out, err := cmd.CombinedOutput(); err != nil {
			// The walk holds on RebootRequired=True; a failed reboot command
			// would deadlock the prep invisibly, so surface it as an event.
			a.logf("reboot command failed: %v: %s", err, out)
			a.emit(context.Background(), corev1.EventTypeWarning, "RebootFailed",
				fmt.Sprintf("reboot command %q failed: %v: %s", a.rebootCommand, err, strings.TrimSpace(string(out))))
		}
	}()
}

// rebootRequest is one step body's reboot request, held until the
// checkpoint fires it. pci ("" for host-wide requests) attributes the
// request to the device that raised it: dropPendingReboots can then void
// one function's stale request without eating a sibling function's fresh
// one, regardless of the order the step loop processed them in.
type rebootRequest struct {
	reason  string
	message string
	pci     string
}

// requestRebootBg lets step bodies (which carry no context) request a
// reboot. The request is RECORDED, not executed: checkpointReboot fires it
// once the stage pass goes quiet — bash semantics (v105 accumulates
// needs_reboot across a block and reboots after the block finishes).
// Firing at request time raced multi-cycle steps live: grub's request
// armed the 60s timer while aptPackages was still configuring doca-all,
// the grace expired mid-dpkg, and the reboot left dpkg interrupted —
// aptPackages then burned its retry budget and failed the NodePrep
// (bl-r1-c2-02, 0.1.38). Requests dedup by reason because blocked steps
// re-request on every cycle while the walk converges around them.
func (a *Agent) requestRebootBg(reason, message string, pci string) {
	for _, r := range a.pendingReboot {
		if r.reason == reason && r.pci == pci {
			return
		}
	}
	a.pendingReboot = append(a.pendingReboot, rebootRequest{reason: reason, message: message, pci: pci})
	a.emit(context.Background(), corev1.EventTypeNormal, "RebootRequested",
		fmt.Sprintf("%s: %s (reboot fires when the walk goes quiet)", reason, message))
}

// dropPendingReboots discards recorded requests of one reason without
// firing them. The cold halt of the SR-IOV stage machine uses it: a warm
// load request recorded before the machine learned the load needs a cold
// power cycle must never fire from a later quiet checkpoint — that is the
// second reboot 0.1.51 issued live on bl-r1-c2-06 after ColdRebootRequired
// was already set.
func (a *Agent) dropPendingReboots(reason string, pci string) {
	kept := a.pendingReboot[:0]
	for _, r := range a.pendingReboot {
		if r.reason == reason && (pci == "" || r.pci == "" || r.pci == pci) {
			continue
		}
		kept = append(kept, r)
	}
	a.pendingReboot = kept
}

// checkpointReboot is the bash needs_reboot checkpoint: after a stage pass,
// if reboot requests have accumulated and NOTHING changed state this pass,
// the walk is stuck without a fresh boot — fire one reboot carrying every
// pending request (doca install + grub cmdline + ib_core netns land in a
// single boot, exactly the grouping the bash performs). A pass that made
// progress — a step converged, a failed step is retrying — keeps the walk
// going; the pending requests ride along and fire at the first quiet pass.
// Reaching Ready with a pending request is legitimate: if every step
// converged without needing the fresh boot (e.g. the firmware ceiling
// already covered the wanted VF count), the NV change simply applies at
// the node's next natural reboot and nothing fires.
func (a *Agent) checkpointReboot(ctx context.Context) {
	if a.stageProgressed || a.rebootIssued || len(a.pendingReboot) == 0 {
		return
	}
	reason, message := aggregateReboots(a.pendingReboot)
	a.pendingReboot = nil
	a.requestReboot(ctx, reason, message)
}

// aggregateReboots folds pending requests into one condition: the first
// reason is the machine-readable token (metav1 reasons are plain tokens),
// the full set names itself in the message.
func aggregateReboots(reqs []rebootRequest) (reason, message string) {
	reasons := make([]string, 0, len(reqs))
	msgs := make([]string, 0, len(reqs))
	for _, r := range reqs {
		reasons = append(reasons, r.reason)
		msgs = append(msgs, r.message)
	}
	if len(reasons) == 1 {
		return reasons[0], msgs[0]
	}
	return reasons[0], fmt.Sprintf("%s: %s", strings.Join(reasons, "+"), strings.Join(msgs, " | "))
}

var _ = unstructured.Unstructured{}
