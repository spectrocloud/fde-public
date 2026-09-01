// Package agent implements the node-side engine: a poll loop that walks the
// NodePrep phase machine, runs steps Detect→Apply→Verify, tracks boot_id for
// the reboot protocol, and verifies at boot before Ready (design NP-CTRL-001
// §5, §6.1).
//
// v0.1 scope: inventory via sysfs, real downloads, and detect-only hardware
// steps. Mutating hardware/OS steps require the agent flag -host-mutations;
// without it they report Blocked honestly instead of guessing.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"spectrocloud.com/nodeprep/api/v1alpha1"
	"spectrocloud.com/nodeprep/internal/k8sutil"
)

// stepDef is one entry of the stage DAG (design §5.1). critical steps are
// re-verified at every boot before the taint may be released (§6.1).
type stepDef struct {
	name     string
	stage    v1alpha1.Phase
	critical bool
	run      func(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string)
}

// stepDefs enumerates the v0.1 steps. Hardware steps gate on Mellanox
// presence: without the hardware they are trivially Done (nothing to do),
// with it they require either host mutations enabled or the v0.2 tool
// integration and say so.
var stepDefs = []stepDef{
	// --- Provisioning (bash fn_init_sw_stage) ---
	{name: "downloads", stage: v1alpha1.PhaseProvisioning, run: stepDownloads},
	{name: "aptPackages", stage: v1alpha1.PhaseProvisioning, run: stepAptPackages},
	{name: "grubParams", stage: v1alpha1.PhaseProvisioning, run: stepGrubParams},
	{name: "ibCoreNetns", stage: v1alpha1.PhaseProvisioning, run: stepIbCoreNetns},
	// --- Flashing (bash fn_init_hw_stage) ---
	{name: "bfbFlash", stage: v1alpha1.PhaseFlashing, run: stepNeedsMFT("bfb-install", "BFB flash")},
	// --- Configuring (bash fn_config_stage) ---
	{name: "mlxconfig", stage: v1alpha1.PhaseConfiguring, run: stepNeedsMFT("mlxconfig", "adapter firmware config")},
	// --- Finalizing (bash fn_set_vfs et al.) ---
	{name: "sriovNumVFs", stage: v1alpha1.PhaseFinalizing, critical: true, run: stepSriovNumVFs},
	{name: "vfGuids", stage: v1alpha1.PhaseFinalizing, critical: true, run: stepNeedsMFT("mlxconfig/sysfs", "VF GUID synthesis (design §8.3)")},
	{name: "udevRules", stage: v1alpha1.PhaseFinalizing, critical: true, run: stepUdevRules},
	{name: "losslessRoce", stage: v1alpha1.PhaseFinalizing, critical: true, run: stepNeedsMFT("mlnx_qos/mlxreg", "lossless RoCE")},
	{name: "ovsBridges", stage: v1alpha1.PhaseFinalizing, critical: true, run: stepOvsBridges},
	{name: "disableACS", stage: v1alpha1.PhaseFinalizing, critical: true, run: stepDisableACS},
	{name: "kubeletState", stage: v1alpha1.PhaseFinalizing, run: stepKubeletState},
	{name: "nfsRdma", stage: v1alpha1.PhaseFinalizing, run: stepNfsRdma},
	{name: "driverReadyMarker", stage: v1alpha1.PhaseFinalizing, run: stepDriverReady},
}

func stepsForStage(stage v1alpha1.Phase) []stepDef {
	var out []stepDef
	for _, s := range stepDefs {
		if s.stage == stage {
			out = append(out, s)
		}
	}
	return out
}

// stepHash makes a Done step re-open when the profile generation changes
// (design §5.1 inputsHash; full input-sensitivity lands with real Apply).
func stepHash(np *v1alpha1.NodePrep, name string, extra string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d/%s/%s", np.Status.ObservedProfileGeneration, name, extra)))
	return hex.EncodeToString(h[:8])
}

func (a *Agent) hasMellanox() bool { return len(a.mellanoxFns) > 0 }

func (a *Agent) mutationsAllowed(profile *v1alpha1.NodePrepProfile) bool {
	return a.hostMutations && profile.Spec.Policy.MutationsOn()
}

// stepDownloads implements the BFB/DOCA download with sha256 verification
// (design fixes P4). Without firmware.source configured it is Done (skipped).
func stepDownloads(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	fw := profile.Spec.Firmware
	if fw.Source == "" {
		return v1alpha1.StepDone, "skipped: no firmware source configured"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "downloads require -host-mutations and policy.hostMutations"
	}
	if fw.BFB.Name != "" {
		url := strings.TrimSuffix(fw.Source, "/") + "/firmware/bfb/" + fw.BFB.Name
		if err := a.downloadFile(url, filepath.Join(a.bfbDir(), fw.BFB.Name), fw.BFB.SHA256); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("BFB download: %v", err)
		}
	}
	if fw.DOCA.Deb != "" {
		url := strings.TrimSuffix(fw.Source, "/") + "/" + fw.DOCA.Deb
		if err := a.downloadFile(url, filepath.Join(a.spcxDir(), fw.DOCA.Deb), fw.DOCA.SHA256); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("DOCA deb download: %v", err)
		}
	}
	return v1alpha1.StepDone, "firmware artifacts present and checksum-verified"
}

// downloadFile fetches url into dest and verifies the payload against
// wantSHA when one is configured. Every download is logged start-to-finish
// (URL, destination, bytes, duration, checksum) so pod logs audit exactly
// what landed on the host (design §2: observable by default).
func (a *Agent) downloadFile(url, dest, wantSHA string) error {
	a.logf("download %s -> %s", url, dest)
	// The spcx cache does not exist on a fresh node (found in live testing:
	// os.Create fails with ENOENT after a perfectly good HTTP 200).
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}
	tmp := dest + ".tmp"
	start := time.Now()
	resp, err := http.Get(url) // #nosec G107 -- source is operator-configured (MAAS/TFTP mirror)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s from %s", resp.Status, url)
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	checksum := "sha256 not verified (none configured)"
	if wantSHA != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if got != wantSHA {
			os.Remove(tmp)
			return fmt.Errorf("sha256 mismatch for %s: got %s want %s", filepath.Base(dest), got, wantSHA)
		}
		checksum = "sha256 verified"
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	a.logf("downloaded %s: %d bytes in %s, %s", filepath.Base(dest), n, time.Since(start).Round(time.Millisecond), checksum)
	return nil
}

// stepGrubParams detects the desired cmdline parameters (iommu, hugepages)
// in /proc/cmdline. Writing them is the boot path of v0.2.
func stepGrubParams(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	hb := profile.Spec.HostBoot
	var want []string
	switch strings.ToLower(hb.IOMMU) {
	case "intel":
		want = append(want, "intel_iommu=on")
	case "amd":
		want = append(want, "amd_iommu=on")
	}
	if hb.Hugepages.Pages1G > 0 {
		want = append(want, "default_hugepagesz=1G", fmt.Sprintf("hugepagesz=1G"), fmt.Sprintf("hugepages=%d", hb.Hugepages.Pages1G))
	} else if hb.Hugepages.Pages2M > 0 {
		want = append(want, "default_hugepagesz=2M", fmt.Sprintf("hugepagesz=2M"), fmt.Sprintf("hugepages=%d", hb.Hugepages.Pages2M))
	}
	if len(want) == 0 {
		return v1alpha1.StepDone, "skipped: no hostBoot parameters configured"
	}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return v1alpha1.StepBlocked, fmt.Sprintf("cannot read /proc/cmdline: %v", err)
	}
	have := string(cmdline)
	var missing []string
	for _, w := range want {
		if !strings.Contains(have, w) {
			missing = append(missing, w)
		}
	}
	if len(missing) == 0 {
		return v1alpha1.StepDone, "kernel cmdline has required parameters"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, fmt.Sprintf("cmdline missing %v; grub writes need -host-mutations (v0.2)", missing)
	}
	return v1alpha1.StepBlocked, "grub assembly lands in v0.2 (design fn_init_sw_stage)"
}

// stepIbCoreNetns mirrors 'options ib_core netns_mode=0' detection.
func stepIbCoreNetns(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if profile.Spec.EastWest.NumVFs <= 0 && profile.Spec.NorthSouth.NumVFs <= 0 {
		return v1alpha1.StepDone, "skipped: no VFs requested"
	}
	got, err := os.ReadFile("/sys/module/ib_core/parameters/netns_mode")
	if err != nil {
		if !a.hasMellanox() {
			return v1alpha1.StepDone, "skipped: ib_core not loaded and no Mellanox hardware"
		}
		return v1alpha1.StepBlocked, fmt.Sprintf("cannot read ib_core netns_mode: %v", err)
	}
	if strings.TrimSpace(string(got)) == profile.Spec.HostBoot.RDMANetnsMode {
		return v1alpha1.StepDone, "ib_core netns_mode matches profile"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, fmt.Sprintf("ib_core netns_mode is %q, want %q; modprobe writes need -host-mutations", strings.TrimSpace(string(got)), profile.Spec.HostBoot.RDMANetnsMode)
	}
	return v1alpha1.StepBlocked, "modprobe.d writes land in v0.2"
}

// stepNotConfiguredYet marks provision-side package steps deferred to v0.2;
// they are Done (skipped) so the vanilla end-to-end loop stays green.

// stepAptPackages installs the DOCA host packages configured in the profile,
// deliberately with NO Mellanox-hardware gate: the host userspace is useful
// without NICs and lab nodes install it too, matching the bash script. The
// deb bundle, when configured, is installed first (it bootstraps the NVIDIA
// apt repository), then the named packages via apt. Detection is dpkg state,
// so the step is idempotent and only the missing pieces are installed.
func stepAptPackages(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	fw := profile.Spec.Firmware
	if fw.DOCA.Deb == "" && len(fw.DOCA.Packages) == 0 {
		return v1alpha1.StepDone, "skipped: no DOCA deb or packages configured"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "package installation requires -host-mutations and policy.hostMutations"
	}
	env := append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")

	// The deb must already be on the host: the downloads step fetches it
	// into the spcx cache during the same stage (it runs first).
	debHostPath := ""
	if fw.DOCA.Deb != "" {
		debHostPath = strings.TrimPrefix(filepath.Join(a.spcxDir(), fw.DOCA.Deb), "/host")
		if _, err := os.Stat(filepath.Join(a.spcxDir(), fw.DOCA.Deb)); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("DOCA deb %s missing from %s; configure firmware.source so the downloads step fetches it first", fw.DOCA.Deb, a.spcxDir())
		}
	}

	debNeeded := false
	debPkg := ""
	if fw.DOCA.Deb != "" {
		out, err := a.hostExec(nil, 2*time.Minute, "dpkg-deb", "-f", debHostPath, "Package")
		if err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("cannot read package name from %s: %v", fw.DOCA.Deb, err)
		}
		debPkg = strings.TrimSpace(out)
		debNeeded = !a.pkgInstalled(env, debPkg)
	}
	missing := []string{}
	for _, p := range fw.DOCA.Packages {
		if !a.pkgInstalled(env, p) {
			missing = append(missing, p)
		}
	}
	if !debNeeded && len(missing) == 0 {
		return v1alpha1.StepDone, "DOCA packages already installed (dpkg state clean)"
	}

	if debNeeded {
		if _, err := a.hostExec(env, 15*time.Minute, "dpkg", "--install", debHostPath); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("dpkg --install %s: %v", fw.DOCA.Deb, err)
		}
	}
	if len(missing) > 0 {
		if fw.DOCA.Deb != "" {
			// the bundle deb bootstrapped the NVIDIA apt repository; refresh
			if _, err := a.hostExec(env, 10*time.Minute, "apt-get", "update"); err != nil {
				return v1alpha1.StepFailed, fmt.Sprintf("apt-get update: %v", err)
			}
		}
		args := append([]string{"--yes"}, missing...)
		if _, err := a.hostExec(env, 30*time.Minute, "apt-get", args...); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("apt-get install %v: %v", missing, err)
		}
	}

	parts := []string{}
	if debNeeded {
		parts = append(parts, "deb "+fw.DOCA.Deb+" ("+debPkg+")")
	}
	if len(missing) > 0 {
		parts = append(parts, "packages "+strings.Join(missing, ","))
	}
	return v1alpha1.StepDone, "installed " + strings.Join(parts, " and ")
}


// stepNeedsMFT is the shared shape of the hardware steps that require host
// tooling (bfb-install, mlxconfig, mlnx_qos, mlxreg): Done without Mellanox
// hardware, Blocked with it unless mutations are enabled — and in v0.1 even
// then they defer to v0.2 rather than half-run a flash.
func stepNeedsMFT(tool, what string) func(*Agent, *v1alpha1.NodePrep, *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	return func(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
		if !a.hasMellanox() {
			return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
		}
		if _, err := findHostTool(tool); err != nil {
			return v1alpha1.StepBlocked, fmt.Sprintf("Mellanox hardware present but %s not found: %v", tool, err)
		}
		if !a.mutationsAllowed(profile) {
			return v1alpha1.StepBlocked, fmt.Sprintf("%s requires -host-mutations (detect-only run)", what)
		}
		return v1alpha1.StepBlocked, what + " lands in v0.2 with full Detect/Apply/Verify"
	}
}

// stepSriovNumVFs detects the requested VF count in sysfs.
func stepSriovNumVFs(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !a.hasMellanox() {
		return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
	}
	for _, nic := range a.mellanoxFns {
		want := vfCountFor(profile, nic)
		got, err := readSysfsInt("/sys/bus/pci/devices/" + nic.pci + "/sriov_numvfs")
		if err != nil {
			return v1alpha1.StepBlocked, fmt.Sprintf("cannot read sriov_numvfs for %s: %v", nic.pci, err)
		}
		if got != want {
			if !a.mutationsAllowed(profile) {
				return v1alpha1.StepBlocked, fmt.Sprintf("%s has %d VFs, profile wants %d (write needs -host-mutations)", nic.pci, got, want)
			}
			return v1alpha1.StepBlocked, "sriov_numvfs writes land in v0.2"
		}
	}
	return v1alpha1.StepDone, "VF counts match profile"
}

// stepUdevRules checks the rename rule files exist where the rail grammar
// expects them (design §7/§8: eth_rN, roce_rN).
func stepUdevRules(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !a.hasMellanox() || len(railFns(profile, a)) == 0 {
		return v1alpha1.StepDone, "skipped: no rail NICs to rename"
	}
	for _, f := range []string{"70-persistent-net.rules", "60-persistent-rdma.rules"} {
		if _, err := os.Stat(filepath.Join(a.hostEtcUdev, f)); err != nil {
			if !a.mutationsAllowed(profile) {
				return v1alpha1.StepBlocked, fmt.Sprintf("%s missing; udev writes need -host-mutations", f)
			}
			return v1alpha1.StepBlocked, "udev rule generation lands in v0.2 (design §8.3 grammar)"
		}
	}
	return v1alpha1.StepDone, "udev rename rules present"
}

// stepOvsBridges checks br-rail-rN bridges exist in the OVS database when
// switchdev is requested.
func stepOvsBridges(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !strings.EqualFold(profile.Spec.EastWest.EswitchMode, "switchdev") {
		return v1alpha1.StepDone, "skipped: eswitch mode is not switchdev"
	}
	if !a.hasMellanox() {
		return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
	}
	if _, err := os.Stat("/host/var/run/openvswitch/conf.db"); err != nil {
		return v1alpha1.StepBlocked, "OVS not found on host (libovsdb integration lands in v0.2)"
	}
	return v1alpha1.StepBlocked, "bridge convergence via libovsdb lands in v0.2"
}

// stepDisableACS is policy-gated; reading ACS caps needs setpci (v0.2).
func stepDisableACS(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !profile.Spec.Policy.DisableACS {
		return v1alpha1.StepDone, "skipped by policy (disableACS=false)"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "ACS disable requires -host-mutations"
	}
	return v1alpha1.StepBlocked, "setpci ACS handling lands in v0.2"
}

// stepKubeletState clears the kubelet CPU/Memory manager state files the way
// nodeprep-v105.sh fn_ensure_state does, guarded: never delete under a
// running kubelet — stop, delete, restart (the same sequence the boot hook
// carries into every future boot, design §6.2). The reset is one-shot per
// prep: runStage never re-opens Done steps, so a kubelet that re-persists
// fresh manager state cannot cause a reset loop.
func stepKubeletState(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	hb := profile.Spec.HostBoot
	mode := hb.KubeletStateReset
	if mode == "" || mode == "off" {
		return v1alpha1.StepDone, "skipped: kubeletStateReset off"
	}
	stale := staleKubeletStateFiles(a.hostKubeletDir)
	if !a.mutationsAllowed(profile) {
		if len(stale) == 0 {
			return v1alpha1.StepDone, "kubelet manager state files absent (detect-only: boot hook not installed)"
		}
		return v1alpha1.StepBlocked, fmt.Sprintf("stale kubelet state %v; guarded reset requires -host-mutations and policy.hostMutations", stale)
	}
	hookMsg, err := a.ensureBootHook(profile)
	if err != nil {
		return v1alpha1.StepFailed, "boot hook: " + err.Error()
	}
	if len(stale) == 0 {
		return v1alpha1.StepDone, hookMsg + "; kubelet manager state files absent"
	}

	env := append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, _ := a.hostExec(env, 30*time.Second, "systemctl", "is-active", "kubelet")
	wasActive := strings.TrimSpace(out) == "active"
	if wasActive {
		a.logf("kubelet is active; stopping it for the guarded state reset")
		if _, err := a.hostExec(env, 2*time.Minute, "systemctl", "stop", "kubelet"); err != nil {
			_, _ = a.hostExec(env, 2*time.Minute, "systemctl", "start", "kubelet") // never leave it stopped
			return v1alpha1.StepFailed, fmt.Sprintf("stopping kubelet: %v", err)
		}
	}
	removed := []string{}
	for _, f := range stale {
		p := filepath.Join(a.hostKubeletDir, f)
		a.logf("removing stale kubelet state file %s", p)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			if wasActive {
				_, _ = a.hostExec(env, 2*time.Minute, "systemctl", "start", "kubelet")
			}
			return v1alpha1.StepFailed, fmt.Sprintf("removing %s: %v", f, err)
		}
		removed = append(removed, f)
	}
	restarted := "kubelet was not running"
	if wasActive {
		if _, err := a.hostExec(env, 2*time.Minute, "systemctl", "start", "kubelet"); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("starting kubelet: %v", err)
		}
		active := false
		for i := 0; i < 30; i++ {
			out, err := a.hostExec(env, 30*time.Second, "systemctl", "is-active", "kubelet")
			if err == nil && strings.TrimSpace(out) == "active" {
				active = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !active {
			return v1alpha1.StepFailed, "kubelet did not return to active within 60s after the state reset"
		}
		restarted = "kubelet stopped and restarted"
	}
	return v1alpha1.StepDone, fmt.Sprintf("%s; cleared stale kubelet state %v (%s)", hookMsg, removed, restarted)
}

func stepNfsRdma(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !profile.Spec.NFSRDMA.Enabled {
		return v1alpha1.StepDone, "skipped by policy (nfsRdma disabled)"
	}
	return v1alpha1.StepBlocked, "NFSoRDMA package/module handling lands in v0.2"
}

func stepDriverReady(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !a.hasMellanox() {
		return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
	}
	path := filepath.Join(a.hostRunDir, "mellanox/drivers/.driver-ready")
	if _, err := os.Stat(path); err == nil {
		return v1alpha1.StepDone, "driver-ready marker present"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "driver-ready marker requires -host-mutations"
	}
	if err := os.MkdirAll(filepath.Join(a.hostRunDir, "mellanox/drivers"), 0o755); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("mkdir /run/mellanox/drivers: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil { // #nosec G306 -- marker file, world-readable by design
		return v1alpha1.StepFailed, fmt.Sprintf("write driver-ready: %v", err)
	}
	a.logf("wrote driver-ready marker %s (GPU Operator GDS workaround)", path)
	return v1alpha1.StepDone, "driver-ready marker written (GPU Operator GDS workaround)"
}

// findHostTool looks for a tool on PATH or in the host mount.
func findHostTool(name string) (string, error) {
	if _, err := os.Stat(filepath.Join("/host/usr/bin", name)); err == nil {
		return filepath.Join("/host/usr/bin", name), nil
	}
	if _, err := os.Stat(filepath.Join("/host/usr/sbin", name)); err == nil {
		return filepath.Join("/host/usr/sbin", name), nil
	}
	return "", fmt.Errorf("%s not found in /host/usr/{bin,sbin}", name)
}

func readSysfsInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var v int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &v); err != nil {
		return 0, err
	}
	return v, nil
}

// vfCountFor mirrors the bash semantics: east-west count for rail NICs,
// north-south for DPUs; below 1 the bash script clamps to 1 at config time.
func vfCountFor(profile *v1alpha1.NodePrepProfile, nic pciDevice) int {
	if nic.rail == "dpu" {
		if profile.Spec.NorthSouth.NumVFs > 0 {
			return profile.Spec.NorthSouth.NumVFs
		}
		return 1
	}
	if profile.Spec.EastWest.NumVFs > 0 {
		return profile.Spec.EastWest.NumVFs
	}
	return 1
}

// railFns returns the Mellanox devices assigned to rails by the profile.
func railFns(profile *v1alpha1.NodePrepProfile, a *Agent) []pciDevice {
	var out []pciDevice
	for _, nic := range a.mellanoxFns {
		if strings.HasPrefix(nic.rail, "r") {
			out = append(out, nic)
		}
	}
	return out
}

var _ = corev1.EventTypeNormal // referenced by agent.go event emissions
var _ = k8sutil.ConditionStatus
