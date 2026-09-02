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

	// rebootIssued guards the one-shot reboot execution per boot.
	rebootIssued bool

	// noPrepLogged keeps the "no NodePrep yet" line to one occurrence
	// instead of one per poll cycle.
	noPrepLogged bool

	// backoffUntil paces re-running steps that just Failed (design §5.1
	// retry budget): in-memory, doubling with each consecutive failure,
	// cleared on success and on the resume annotation.
	backoffUntil map[string]time.Time

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
}

// bootVerifyInterval paces the Ready-phase re-verification. A new boot
// always verifies immediately (design §6.1); between boots the drift check
// runs on this maintenance cadence, not every poll cycle.
const bootVerifyInterval = 5 * time.Minute

func New(client kubernetes.Interface, dyn dynamic.Interface, nodeName, ns string, interval time.Duration, allowReboot, hostMutations bool, rebootCommand string) *Agent {
	return &Agent{
		client: client, dyn: dyn, nodeName: nodeName, ns: ns,
		interval: interval, allowReboot: allowReboot, hostMutations: hostMutations,
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

func (a *Agent) setPhase(ctx context.Context, phase v1alpha1.Phase, reason, message string) {
	if err := a.patchStatus(ctx, map[string]interface{}{"phase": string(phase)}); err != nil {
		a.logf("phase patch to %s failed: %v", phase, err)
		return
	}
	a.emit(ctx, corev1.EventTypeNormal, "PhaseTransition", fmt.Sprintf("phase -> %s (%s): %s", phase, reason, message))
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
		if changedBoot {
			a.emit(ctx, corev1.EventTypeNormal, "BootDetected", fmt.Sprintf("boot changed %s -> %s; reboot #%d recorded", np.Status.BootID, bootID, total))
			np.Status.BootID = bootID
			np.Status.Conditions = conds
		}
	}

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

	// Run the current stage.
	switch np.Status.Phase {
	case v1alpha1.PhasePending, v1alpha1.PhaseProvisioning, v1alpha1.PhaseFlashing, v1alpha1.PhaseConfiguring, v1alpha1.PhaseFinalizing:
		a.runStage(ctx, np, profile)
	case v1alpha1.PhaseReady:
		a.verifyReady(ctx, np, profile, bootID)
	case v1alpha1.PhaseFailed:
		// Hold; recovery is the resume annotation (design §5.2).
	default:
		a.emit(ctx, corev1.EventTypeWarning, "UnknownPhase", fmt.Sprintf("unknown phase %q", np.Status.Phase))
	}
}

func (a *Agent) removeAnnotation(ctx context.Context, key string) error {
	patch, _ := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": map[string]interface{}{key: nil}},
	})
	_, err := a.dyn.Resource(nodePrepsGVR).Patch(ctx, a.nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
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
		for i := range mellanox {
			nics[i] = mellanox[i].nicStatus()
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
	phase := np.Status.Phase
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
		if k8sutil.ConditionStatus(np.Status.Conditions, v1alpha1.ConditionMaintenanceAdmitted) == "False" {
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
	if err := a.patchStatus(ctx, map[string]interface{}{"steps": ledger}); err != nil {
		a.logf("step ledger patch failed: %v", err)
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
// Fresh messages are written into the step ledger as it goes — a verify
// pass is the step's latest outcome while Ready, and a provisioning-era
// message would otherwise keep describing state (e.g. a superseded
// inventory classification) for the life of the boot.
func (a *Agent) bootVerify(ctx context.Context, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) bool {
	changed := false
	for _, def := range stepDefs {
		if !def.critical {
			continue
		}
		state, msg := def.run(a, np, profile)
		if s := stepByName(np.Status.Steps, def.name); s != nil && s.Message != msg {
			s.Message = msg
			changed = true
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

// requestReboot records the requirement in status; execution honors the
// -allow-reboot flag (design §5.2 protocol step 3). v0.1 steps do not yet
// set RebootRequired; the path is exercised in the lab.
func (a *Agent) requestReboot(ctx context.Context, reason, message string) {
	conds := []metav1.Condition{}
	k8sutil.SetCondition(&conds, v1alpha1.ConditionRebootRequired, "True", reason, message, 0)
	_ = a.patchStatus(ctx, map[string]interface{}{"conditions": conds})
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
			a.logf("reboot command failed: %v: %s", err, out)
		}
	}()
}

// requestRebootBg lets step bodies (which carry no context) raise the
// RebootRequired condition; requestReboot only needs ctx for the status
// writes, which a background context serves identically.
func (a *Agent) requestRebootBg(reason, message string) {
	a.requestReboot(context.Background(), reason, message)
}

var _ = unstructured.Unstructured{}
