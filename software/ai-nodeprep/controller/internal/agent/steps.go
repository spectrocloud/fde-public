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
	"regexp"
	"strconv"
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
	{name: "aptUpgrade", stage: v1alpha1.PhaseProvisioning, run: stepAptUpgrade},
	{name: "aptPackages", stage: v1alpha1.PhaseProvisioning, run: stepAptPackages},
	{name: "lldpdConfig", stage: v1alpha1.PhaseProvisioning, run: stepLldpdConfig},
	{name: "grubParams", stage: v1alpha1.PhaseProvisioning, run: stepGrubParams},
	{name: "rshimService", stage: v1alpha1.PhaseProvisioning, run: stepRshimService},
	{name: "ibCoreNetns", stage: v1alpha1.PhaseProvisioning, run: stepIbCoreNetns},
	// --- Flashing (bash fn_init_hw_stage) ---
	{name: "bfbFlash", stage: v1alpha1.PhaseFlashing, run: stepBFBFlash},
	// --- Configuring (bash fn_config_stage) ---
	{name: "mlxconfig", stage: v1alpha1.PhaseConfiguring, critical: true, run: stepMlxconfig},
	{name: "netplanMTU", stage: v1alpha1.PhaseConfiguring, critical: true, run: stepFabricNetplan},
	// --- Finalizing (bash fn_set_vfs et al.) ---
	{name: "sriovNumVFs", stage: v1alpha1.PhaseFinalizing, critical: true, run: stepSriovNumVFs},
	{name: "vfGuids", stage: v1alpha1.PhaseFinalizing, critical: true, run: stepVfGuids},
	{name: "udevRules", stage: v1alpha1.PhaseFinalizing, critical: true, run: stepUdevRules},
	{name: "losslessRoce", stage: v1alpha1.PhaseFinalizing, critical: true, run: stepLosslessRoce},
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

// Profile edits re-open steps via the generation check in cycle (design
// §5.1); per-step inputsHash lands with real Apply.

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

// downloadFile ensures url's payload is present at dest with wantSHA
// verified. A file already at dest that hashes to wantSHA is reused as-is —
// the spcx cache lives on the host and survives agent restarts and reboots,
// so re-preps must not re-fetch the 711 MB BFB and 843 MB DOCA deb. Anything
// else (missing, empty, hash-mismatched — e.g. a rotated upstream artifact)
// is fetched fresh via .tmp+rename. A local copy cannot be called known-good
// without a configured checksum, so an empty wantSHA always re-downloads.
// Every download is logged start-to-finish (URL, destination, bytes,
// duration, checksum) so pod logs audit exactly what landed on the host
// (design §2: observable by default).
func (a *Agent) downloadFile(url, dest, wantSHA string) error {
	a.logf("download %s -> %s", url, dest)
	if wantSHA != "" {
		if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
			if got := fileSHA256(dest); got == wantSHA {
				a.logf("reusing %s (%d bytes, sha256 verified) from the local cache", filepath.Base(dest), info.Size())
				return nil
			} else if got != "" {
				a.logf("local %s failed sha256 verification (got %s), re-downloading", filepath.Base(dest), got)
			}
		}
	}
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

// fileSHA256 hashes a file's contents; "" when unreadable (the caller then
// treats the copy as unusable and re-downloads).
func fileSHA256(path string) string {
	f, err := os.Open(path) // #nosec G304 -- path is the step's own cache destination
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
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
	// VFs requested: SRIOV needs the vendor IOMMU on and passthrough on the
	// next boot (bash fn_init_sw_stage L379-402), regardless of hostBoot.
	if profile.Spec.EastWest.NumVFs > 0 || profile.Spec.NorthSouth.NumVFs > 0 {
		switch grubCPUVendor() {
		case "intel":
			want = appendUnique(want, "intel_iommu=on")
		case "amd":
			want = appendUnique(want, "amd_iommu=on")
		}
		want = appendUnique(want, "iommu=pt")
	}
	if hb.Hugepages.Pages1G > 0 {
		want = append(want, "default_hugepagesz=1G", fmt.Sprintf("hugepagesz=1G"), fmt.Sprintf("hugepages=%d", hb.Hugepages.Pages1G))
	} else if hb.Hugepages.Pages2M > 0 {
		want = append(want, "default_hugepagesz=2M", fmt.Sprintf("hugepagesz=2M"), fmt.Sprintf("hugepages=%d", hb.Hugepages.Pages2M))
	}
	if len(want) == 0 {
		return v1alpha1.StepDone, "skipped: no hostBoot parameters configured"
	}
	// A parameter counts as present when the running kernel booted with it
	// or the host grub config already arranges it for the next boot — the
	// bash script checks the config file, not /proc/cmdline, for iommu=pt.
	var missing []string
	for _, w := range want {
		if !grubParamPresent(a, w) {
			missing = append(missing, w)
		}
	}
	if len(missing) == 0 {
		return v1alpha1.StepDone, fmt.Sprintf("kernel cmdline has required parameters (%s)", strings.Join(want, " "))
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, fmt.Sprintf("cmdline missing %v; grub writes need -host-mutations and policy.hostMutations", missing)
	}
	// The drop-in is idempotent by construction: every wanted key is sed-
	// stripped from the inherited cmdline before being re-appended, so a
	// re-run rewrites the same four lines (bash L418-432, verbatim shape).
	dropin := renderGrubDropin(want)
	path := filepath.Join(a.hostEtcDir(), "default/grub.d/90-nodeprep.cfg")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("create grub.d: %v", err)
	}
	if err := os.WriteFile(path, []byte(dropin), 0o644); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("write 90-nodeprep.cfg: %v", err)
	}
	if _, err := a.hostExec(nil, 5*time.Minute, "update-grub"); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("update-grub: %v", err)
	}
	return v1alpha1.StepDone, fmt.Sprintf("wrote 90-nodeprep.cfg (%s) and ran update-grub; reboot applies it", strings.Join(missing, " "))
}

// grubCPUVendor classifies the CPU from /proc/cpuinfo (bash lscpu greps
// "Model name" for intel/amd; vendor_id is the stable equivalent).
func grubCPUVendor() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		v := strings.ToLower(line)
		if strings.HasPrefix(v, "vendor_id") {
			if strings.Contains(v, "genuineintel") || strings.Contains(v, "intel") {
				return "intel"
			}
			if strings.Contains(v, "authenticamd") || strings.Contains(v, "amd") {
				return "amd"
			}
		}
	}
	return ""
}

// appendUnique appends p unless it is already in the list (hostBoot and the
// VF block can both ask for the same iommu parameter).
func appendUnique(list []string, p string) []string {
	for _, x := range list {
		if x == p {
			return list
		}
	}
	return append(list, p)
}

// grubParamPresent reports whether the host already boots, or is already
// arranged to boot, with param: /proc/cmdline for the running kernel,
// /etc/default/grub (GRUB_CMDLINE_LINUX*) and any grub.d drop-in for the
// next one. Word-boundary match so "noiommu=pt" doesn't satisfy "iommu=pt".
func grubParamPresent(a *Agent, param string) bool {
	if b, err := os.ReadFile("/proc/cmdline"); err == nil {
		if grubLineHasParam(string(b), param) {
			return true
		}
	}
	for _, path := range []string{
		filepath.Join(a.hostEtcDir(), "default/grub"),
		filepath.Join(a.hostEtcDir(), "default/grub.d/90-nodeprep.cfg"),
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "GRUB_CMDLINE_LINUX") && grubLineHasParam(line, param) {
				return true
			}
		}
	}
	return false
}

// grubLineHasParam matches the full "key=value" token between boundaries
// (line start/end, whitespace or quote) — bash greps for the literal
// "iommu=pt", so iommu=off or iommu=ptrue must not count as present.
func grubLineHasParam(line, param string) bool {
	return regexp.MustCompile(`(^|[\s"'])` + regexp.QuoteMeta(param) + `($|[\s"'])`).MatchString(line)
}

// renderGrubDropin assembles /etc/default/grub.d/90-nodeprep.cfg exactly as
// bash L418-432: inherit the stock cmdline, sed-strip every managed key,
// re-append the managed key=value pairs, xargs-trim.
func renderGrubDropin(want []string) string {
	clean := ""
	for i, w := range want {
		key := strings.SplitN(w, "=", 2)[0]
		sep := ";"
		if i == len(want)-1 {
			sep = ""
		}
		clean += fmt.Sprintf("s/(^|[[:space:]])%s=[^[:space:]]*//g%s", key, sep)
	}
	pairs := strings.Join(want, " ")
	return fmt.Sprintf(`GRUB_CMDLINE_LINUX=" $GRUB_CMDLINE_LINUX "
GRUB_CMDLINE_LINUX="$(printf '%%s' "$GRUB_CMDLINE_LINUX" | sed -E '%s')"
GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX %s"
GRUB_CMDLINE_LINUX="$(echo "$GRUB_CMDLINE_LINUX" | xargs)"
`, clean, pairs)
}

// stepIbCoreNetns mirrors 'options ib_core netns_mode=0' detection.
// stepIbCoreNetns applies the profile's rdmaNetnsMode as
// 'options ib_core netns_mode=<mode>' (bash writes the same file in its
// SR-IOV block) and reboots when the running module still has the old
// setting. Unlike the bash, the profile drives this directly — a profile
// with rdmaNetnsMode set but zero VFs still gets the semantics it asked for.
func stepIbCoreNetns(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	mode := strings.TrimSpace(profile.Spec.HostBoot.RDMANetnsMode)
	if mode == "" {
		if !a.hasMellanox() {
			return v1alpha1.StepDone, "skipped: no rdmaNetnsMode set and no Mellanox hardware"
		}
		return v1alpha1.StepDone, "skipped: profile does not set rdmaNetnsMode"
	}
	got, err := os.ReadFile("/sys/module/ib_core/parameters/netns_mode")
	if err != nil {
		if !a.hasMellanox() {
			return v1alpha1.StepDone, "skipped: ib_core not loaded and no Mellanox hardware"
		}
		return v1alpha1.StepBlocked, fmt.Sprintf("cannot read ib_core netns_mode: %v", err)
	}
	if normNetnsMode(string(got)) == normNetnsMode(mode) {
		return v1alpha1.StepDone, fmt.Sprintf("ib_core netns_mode=%s matches profile", mode)
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, fmt.Sprintf("ib_core netns_mode is %q, want %q; modprobe writes need -host-mutations", strings.TrimSpace(string(got)), mode)
	}
	conf := "options ib_core netns_mode=" + mode + "\n"
	path := filepath.Join(a.hostEtcDir(), "modprobe.d/ib_core.conf")
	if cur, err := os.ReadFile(path); err != nil || string(cur) != conf {
		if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("write %s: %v", path, err)
		}
		a.logf("ibCoreNetns: wrote %s (running module reports %q)", path, strings.TrimSpace(string(got)))
	}
	// ib_core.ko can ship in the initramfs, whose copy of /etc/modprobe.d was
	// baked at kernel/package install time — often minutes before this write
	// (an aptUpgrade in the same stage rebuilds the initramfs first). A module
	// loaded from the initramfs never sees this file, so a bare reboot comes
	// back with the compiled-in default and the step would loop. Refresh the
	// snapshot so the early-load path reads the option too, and only then
	// request the reboot.
	env := append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	if out, err := a.heavyHostExec(env, 10*time.Minute, "update-initramfs", "-u"); err != nil {
		return v1alpha1.StepBlocked, fmt.Sprintf("update-initramfs -u failed (%v); reboot deferred: %s",
			err, strings.TrimSpace(out))
	}
	a.requestRebootBg(v1alpha1.RebootIbCoreNetns,
		fmt.Sprintf("ib_core netns_mode is %q, want %q; reboot required to reload ib_core", normNetnsMode(string(got)), normNetnsMode(mode)))
	return v1alpha1.StepBlocked, "modprobe.d updated and initramfs refreshed; reboot requested to reload ib_core"
}

// normNetnsMode folds the parameter spellings together: sysfs reports Y/N,
// modprobe.d and the profile speak 0/1.
func normNetnsMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "y", "yes", "1", "true":
		return "1"
	case "n", "no", "0", "false":
		return "0"
	}
	return strings.TrimSpace(v)
}

// stepNotConfiguredYet marks provision-side package steps deferred to v0.2;
// they are Done (skipped) so the vanilla end-to-end loop stays green.

// stepAptPackages installs the DOCA host packages configured in the profile,
// deliberately with NO Mellanox-hardware gate: the host userspace is useful
// without NICs and lab nodes install it too, matching the bash script. The
// deb bundle, when configured, is installed first (it bootstraps the NVIDIA
// apt repository), then the named packages via apt. Detection is dpkg state,
// so the step is idempotent and only the missing pieces are installed.
// kernelRelease returns the host's uname -r, cached for the process
// lifetime: a nodeprep never crosses a kernel change without a reboot, and
// a reboot re-verifies the boot before any step advances.
func (a *Agent) kernelRelease(env []string) (string, error) {
	if a.krelKnown {
		return a.krel, nil
	}
	out, err := a.hostExec(env, 30*time.Second, "uname", "-r")
	if err != nil {
		return "", err
	}
	a.krel, a.krelKnown = strings.TrimSpace(out), true
	return a.krel, nil
}

// expandPkg expands the one supported shell-style placeholder in profile
// package names — $(uname -r), e.g. linux-headers-$(uname -r) — from the
// host kernel release. The agent execs argv arrays, never a shell, so any
// other shell expression cannot work and is refused here rather than
// handed to apt as literal text (found live: apt exit 100 on
// "linux-headers-$(uname -r)" before expansion existed).
func expandPkg(pkg, krel string) (string, error) {
	out := strings.ReplaceAll(pkg, "$(uname -r)", krel)
	if strings.ContainsAny(out, "$`") {
		return "", fmt.Errorf("unsupported shell expression (only $(uname -r) is expanded)")
	}
	return out, nil
}

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

	// Package names may reference the running kernel (the DOCA/DKMS build
	// needs the matching headers): expand $(uname -r) before detect or
	// install touches them.
	krel, err := a.kernelRelease(env)
	if err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("reading kernel release for package expansion: %v", err)
	}
	pkgs := make([]string, 0, len(fw.DOCA.Packages))
	for _, p := range fw.DOCA.Packages {
		x, err := expandPkg(p, krel)
		if err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("package %q: %v", p, err)
		}
		pkgs = append(pkgs, x)
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
	for _, p := range pkgs {
		if !a.pkgInstalled(env, p) {
			missing = append(missing, p)
		}
	}
	if !debNeeded && len(missing) == 0 {
		return v1alpha1.StepDone, "DOCA packages already installed (dpkg state clean)"
	}

	if debNeeded {
		if _, err := a.heavyHostExec(env, 15*time.Minute, "dpkg", "--install", debHostPath); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("dpkg --install %s: %v", fw.DOCA.Deb, err)
		}
	}
	if len(missing) > 0 {
		if fw.DOCA.Deb != "" {
			// the bundle deb bootstrapped the NVIDIA apt repository; refresh
			if _, err := a.heavyHostExec(env, 10*time.Minute, "apt-get", "update"); err != nil {
				return v1alpha1.StepFailed, fmt.Sprintf("apt-get update: %v", err)
			}
		}
		args := append([]string{"install", "--yes"}, missing...)
		if _, err := a.heavyHostExec(env, 30*time.Minute, "apt-get", args...); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("apt-get install %v: %v", missing, err)
		}
	}

	// Bash-faithful guard (v105 L325): hold the running kernel's versioned
	// headers after install so later kernel churn or autoremove cannot
	// remove them out from under the running kernel.
	held := []string{}
	for _, p := range missing {
		if !strings.HasPrefix(p, "linux-headers-") {
			continue
		}
		if _, err := a.hostExec(env, 2*time.Minute, "apt-mark", "hold", p); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("apt-mark hold %s: %v", p, err)
		}
		held = append(held, p)
	}

	parts := []string{}
	if debNeeded {
		parts = append(parts, "deb "+fw.DOCA.Deb+" ("+debPkg+")")
	}
	if len(missing) > 0 {
		parts = append(parts, "packages "+strings.Join(missing, ","))
	}
	if len(held) > 0 {
		parts = append(parts, "held "+strings.Join(held, ","))
	}
	return v1alpha1.StepDone, "installed " + strings.Join(parts, " and ")
}

// stepLldpdConfig implements bash v105 L363-368: write the rcp LLDPD config
// (/etc/lldpd.d/rcp-lldpd.conf, 0644) and enable lldpd. The bash relies on
// lldpd's package postinst having started it; here a (re)written config is
// followed by a restart so it takes effect without waiting for a boot.
// lldpd itself must come from the profile's package list — a missing unit
// fails the step with that pointer rather than being silently skipped.
func stepLldpdConfig(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	conf := "configure system hostname .\nconfigure lldp portidsubtype ifname\n"
	dir := filepath.Join(a.hostEtcDir(), "lldpd.d")
	path := filepath.Join(dir, "rcp-lldpd.conf")
	env := os.Environ()

	needWrite := true
	if b, err := os.ReadFile(path); err == nil && string(b) == conf {
		if fi, statErr := os.Stat(path); statErr == nil && fi.Mode().Perm() == 0o644 {
			needWrite = false
		}
	}
	_, enableErr := a.hostExec(env, 30*time.Second, "systemctl", "is-enabled", "--quiet", "lldpd")
	needEnable := enableErr != nil

	if !needWrite && !needEnable {
		return v1alpha1.StepDone, "lldpd configured (unchanged)"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "lldpd config requires -host-mutations and policy.hostMutations"
	}
	if needWrite {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("writing %s: %v", path, err)
		}
	}
	if needEnable {
		if _, err := a.hostExec(env, 30*time.Second, "systemctl", "enable", "lldpd"); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("enabling lldpd (is lldpd in firmware.doca.packages?): %v", err)
		}
	}
	if needWrite {
		// lldpcli consumes /etc/lldpd.d at service start; restart so the new
		// config applies now, not only at the next boot.
		if _, err := a.hostExec(env, 2*time.Minute, "systemctl", "restart", "lldpd"); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("restarting lldpd to apply the config: %v", err)
		}
	}
	if !needWrite {
		return v1alpha1.StepDone, "lldpd enabled"
	}
	return v1alpha1.StepDone, "rcp-lldpd.conf written, lldpd enabled"
}

// stepRshimService implements bash v105 L452-460: rshim enabled and running
// (daemon-reload → enable → restart → verify active), so the BlueField
// flash path in the next stage finds a live rshim. An already-enabled,
// already-active service is left untouched (the bash restarts unconditionally
// every run; the controller's steps are detect-first and idempotent). A node
// without the unit — mft not in the package list, or no rshim in the image —
// skips honestly: there is nothing to enable.
func stepRshimService(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	env := os.Environ()
	out, err := a.hostExec(env, 30*time.Second, "systemctl", "is-enabled", "rshim")
	if err != nil && strings.Contains(out, "No such file") {
		// systemctl prints the missing-unit complaint to stderr, which
		// CombinedOutput merges into out.
		return v1alpha1.StepDone, "skipped: rshim unit not present (install mft via firmware.doca.packages)"
	}
	enabled := err == nil && strings.TrimSpace(out) == "enabled"
	activeOut, activeErr := a.hostExec(env, 30*time.Second, "systemctl", "is-active", "rshim")
	active := activeErr == nil && strings.TrimSpace(activeOut) == "active"
	if enabled && active {
		return v1alpha1.StepDone, "rshim enabled and active (unchanged)"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, fmt.Sprintf("rshim needs enable=%v restart=%v; requires -host-mutations and policy.hostMutations", !enabled, !active)
	}
	if _, err := a.hostExec(env, 30*time.Second, "systemctl", "daemon-reload"); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("daemon-reload: %v", err)
	}
	if !enabled {
		if _, err := a.hostExec(env, 30*time.Second, "systemctl", "enable", "rshim"); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("enabling rshim: %v", err)
		}
	}
	if _, err := a.hostExec(env, 2*time.Minute, "systemctl", "restart", "rshim"); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("restarting rshim: %v", err)
	}
	// The bash sleeps 10s then greps status; poll instead, bounded.
	for i := 0; i < 10; i++ {
		out, err := a.hostExec(env, 30*time.Second, "systemctl", "is-active", "rshim")
		if err == nil && strings.TrimSpace(out) == "active" {
			return v1alpha1.StepDone, "rshim enabled and active"
		}
		time.Sleep(2 * time.Second)
	}
	return v1alpha1.StepFailed, fmt.Sprintf("rshim not active 20s after restart: %s", strings.TrimSpace(activeOut))
}

// stepAptUpgrade implements the bash APT_UPDATE gate (nodeprep-v105.sh
// fn_init_sw_stage): bring package lists current, then upgrade the system
// with conffiles preserved on conflict. Runs before the DOCA packages step
// (the bash upgrades first too). apt work is heavy — host systemd unit —
// and NEEDRESTART_MODE=l keeps services from restarting mid-prep. Detection
// is the upgrade simulation after the list refresh: nothing upgradable
// reads as already current, so re-runs are cheap.
func stepAptUpgrade(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !profile.Spec.Firmware.AptUpgrade {
		return v1alpha1.StepDone, "skipped by policy (firmware.aptUpgrade=false)"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "apt upgrade requires -host-mutations and policy.hostMutations"
	}
	env := append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l")
	if _, err := a.heavyHostExec(env, 10*time.Minute, "apt-get", "update"); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("apt-get update: %v", err)
	}
	sim, err := a.hostExec(env, 5*time.Minute, "apt-get", "-s", "upgrade")
	if err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("apt-get -s upgrade: %v", err)
	}
	upgradable := parseUpgradableCount(sim)
	if upgradable == 0 {
		return v1alpha1.StepDone, "apt update completed; 0 packages upgradable"
	}
	if _, err := a.heavyHostExec(env, 60*time.Minute, "apt-get", "upgrade", "--yes", "--quiet",
		"-o", "Dpkg::Options::=--force-confold"); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("apt-get upgrade (%d packages): %v", upgradable, err)
	}
	return v1alpha1.StepDone, fmt.Sprintf("apt update + upgrade completed (%d packages upgraded)", upgradable)
}

// parseUpgradableCount reads "N upgraded" from apt-get's plan summary.
func parseUpgradableCount(simOut string) int {
	for _, ln := range strings.Split(simOut, "\n") {
		if i := strings.Index(ln, " upgraded"); i > 0 {
			f := strings.Fields(strings.TrimSpace(ln[:i]))
			if len(f) > 0 {
				if n, err := strconv.Atoi(f[len(f)-1]); err == nil {
					return n
				}
			}
		}
	}
	return 0
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

// stepSriovNumVFs converges every Mellanox function's sriov_numvfs onto the
// profile value (bash fn_set_vfs L611: all ConnectX-class functions take
// NUMVF_EW, DPUs NUMVF_NS — no rail restriction; 0 = none at the OS level).
// A count change tears down first (the kernel rejects N→M while VFs exist),
// then writes the target, then waits for the count to settle. The firmware
// ceiling (sriov_totalvfs, raised by the mlxconfig step's NUM_OF_VFS and its
// apply reboot) gates the write: too low reports Blocked until that reboot
// lands.
// totalvfsAdvice explains a too-low sriov_totalvfs by consulting the
// firmware NV config (the same mlxconfig query the mlxconfig step runs):
// either the NUM_OF_VFS/PF_NUM_OF_VF_VALID change is not in NV yet — the
// mlxconfig step's apply reboot will land it — or it IS in NV but the
// running firmware never loaded it, which on this firmware class takes a
// cold power cycle (found live on a Supermicro LOM, FW 14.32.1010: two warm
// reboots with NUM_OF_VFS=4 in NV left TotalVFs=1 in the PCIe SR-IOV
// capability). When mlxconfig is unavailable the advice falls back to the
// generic pointer at the mlxconfig step.
func (a *Agent) totalvfsAdvice(nic pciDevice, want int) string {
	if _, err := findHostTool("mlxconfig"); err != nil {
		return "the mlxconfig step raises NUM_OF_VFS and its apply reboot lands it"
	}
	vals, err := a.mlxconfigGetAll(nic.pci)
	if err != nil {
		return fmt.Sprintf("mlxconfig query failed (%v); the mlxconfig step raises NUM_OF_VFS and its apply reboot lands it", err)
	}
	nv, hasNV := vals["NUM_OF_VFS"]
	pfValid := vals["PF_NUM_OF_VF_VALID"]
	nvN, nvErr := strconv.Atoi(nv)
	if hasNV && nvErr == nil && nvN >= want && (pfValid == "" || pfValid == "1" || pfValid == "True") {
		return fmt.Sprintf("firmware NV already carries NUM_OF_VFS=%d with PF_NUM_OF_VF_VALID enabled, but the running firmware has not loaded it — warm reboots do not reload SR-IOV NV settings on this firmware class, so it needs a cold power cycle (AC) via the fleet/IPMI tooling", nvN)
	}
	if pfValid == "0" || pfValid == "False" {
		return fmt.Sprintf("firmware NV has PF_NUM_OF_VF_VALID=%s, which makes the firmware IGNORE NUM_OF_VFS — the mlxconfig step sets the flag alongside NUM_OF_VFS and its apply reboot lands both", pfValid)
	}
	return fmt.Sprintf("firmware NV carries NUM_OF_VFS=%s; the mlxconfig step raises it to %d and its apply reboot lands it", nv, want)
}

func stepSriovNumVFs(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !a.hasMellanox() {
		return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
	}
	changed := []string{}
	for _, nic := range a.mellanoxFns {
		want := vfCountFor(profile, nic)
		base := "/sys/bus/pci/devices/" + nic.pci
		got, err := readSysfsInt(base + "/sriov_numvfs")
		if err != nil {
			return v1alpha1.StepBlocked, fmt.Sprintf("cannot read sriov_numvfs for %s: %v", nic.pci, err)
		}
		if got == want {
			continue
		}
		if !a.mutationsAllowed(profile) {
			return v1alpha1.StepBlocked, fmt.Sprintf("%s has %d VFs, profile wants %d (write needs -host-mutations)", nic.pci, got, want)
		}
		if total, err := readSysfsInt(base + "/sriov_totalvfs"); err == nil && want > total {
			return v1alpha1.StepBlocked, fmt.Sprintf("%s: profile wants %d VFs but the running firmware exposes %d (sriov_totalvfs): %s", nic.pci, want, total, a.totalvfsAdvice(nic, want))
		}
		if nic.netdev == "" {
			return v1alpha1.StepFailed, fmt.Sprintf("%s has no netdev; cannot set sriov_numvfs", nic.pci)
		}
		// The bash creates VFs on an administratively up interface.
		if out, err := a.hostExec(nil, 30*time.Second, "ip", "-br", "link", "show", "dev", nic.netdev); err == nil {
			f := strings.Fields(out)
			if len(f) > 1 && f[1] != "UP" && f[1] != "UNKNOWN" {
				if _, err := a.hostExec(nil, 30*time.Second, "ip", "link", "set", nic.netdev, "up"); err != nil {
					return v1alpha1.StepFailed, fmt.Sprintf("setting %s up: %v", nic.netdev, err)
				}
			}
		}
		if got > 0 {
			// VF teardown before a resize; the fresh PF then takes the new count.
			if err := a.writeSysfs(base+"/sriov_numvfs", "0\n"); err != nil {
				return v1alpha1.StepFailed, fmt.Sprintf("tearing down VFs on %s: %v", nic.pci, err)
			}
		}
		if err := a.writeSysfs(base+"/sriov_numvfs", strconv.Itoa(want)+"\n"); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("writing sriov_numvfs=%d on %s: %v", want, nic.pci, err)
		}
		settled := false
		for i := 0; i < 30; i++ {
			if v, err := readSysfsInt(base + "/sriov_numvfs"); err == nil && v == want {
				settled = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !settled {
			return v1alpha1.StepFailed, fmt.Sprintf("sriov_numvfs on %s still not %d 15s after write", nic.pci, want)
		}
		changed = append(changed, fmt.Sprintf("%s %d→%d", nic.pci, got, want))
	}
	if len(changed) == 0 {
		return v1alpha1.StepDone, fmt.Sprintf("VF counts match profile (%d VFs)", vfCountFor(profile, a.mellanoxFns[0]))
	}
	return v1alpha1.StepDone, "set sriov_numvfs: " + strings.Join(changed, ", ")
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

// stepKubeletState clears the kubelet CPU/Memory manager state files the way
// nodeprep-v105.sh fn_ensure_state does, guarded: never delete under a
// running kubelet — stop, delete, restart (the same sequence the boot hook
// carries into every future boot, design §6.2). The reset is one-shot per
// prep: runStage never re-opens Done steps, so a kubelet that re-persists
// fresh manager state cannot cause a reset loop.
func stepKubeletState(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	hb := profile.Spec.HostBoot
	mode := hb.KubeletStateReset
	resetWanted := mode == "always" || mode == "readyCheck"
	// The boot hook carries two duties (design §6.2): the guarded kubelet
	// manager-state reset and the mlnx_interface_mgr wait. Install it when
	// either is configured — not only for the kubelet reset.
	hookWanted := hb.BootHookOn() && (resetWanted || hb.MlnxInterfaceMgr == "wait" || hb.MlnxInterfaceMgr == "disable")
	if !resetWanted && !hookWanted {
		return v1alpha1.StepDone, "skipped: no boot duties configured (kubeletStateReset off, mlnxInterfaceMgr off)"
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
	if !resetWanted {
		return v1alpha1.StepDone, hookMsg
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

// stepNfsRdma implements the bash fn_setup_nfsrdma: ensure nfs-common,
// install the OFED-compatible mlnx-nfsrdma-dkms package (requires the DOCA
// apt repository — the aptPackages step bootstraps it in the same prep),
// load the RDMA NFS transport modules, and persist them in
// /etc/modules-load.d so every future boot loads them too. A module that
// refuses to load is the bash's WARN-and-continue case (a reboot after the
// dkms install usually settles it) — reported in the message, not a
// failure.
func stepNfsRdma(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !profile.Spec.NFSRDMA.Enabled {
		return v1alpha1.StepDone, "skipped by policy (nfsRdma disabled)"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "NFSoRDMA installation requires -host-mutations and policy.hostMutations"
	}
	env := append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l")
	confPath := filepath.Join(a.hostEtcDir(), "modules-load.d", "nfsrdma.conf")
	confWant := "rpcrdma\nxprtrdma\nsvcrdma\n"

	// Detect: both packages, the module, and the auto-load config.
	nfsCommon := a.pkgInstalled(env, "nfs-common")
	dkms := a.pkgInstalled(env, "mlnx-nfsrdma-dkms")
	confNow, confErr := os.ReadFile(confPath)
	confOK := confErr == nil && string(confNow) == confWant
	loaded := a.moduleLoaded("rpcrdma")
	if nfsCommon && dkms && confOK && loaded {
		return v1alpha1.StepDone, "NFSoRDMA verified: nfs-common, mlnx-nfsrdma-dkms installed; rpcrdma loaded; modules-load.d/nfsrdma.conf present"
	}

	var did []string
	if !nfsCommon {
		if _, err := a.heavyHostExec(env, 15*time.Minute, "apt-get", "install", "--yes", "nfs-common"); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("apt-get install nfs-common: %v", err)
		}
		did = append(did, "nfs-common installed")
	}
	if !dkms {
		// The package ships with the DOCA repository; without it apt fails
		// and the message points at the real cause.
		if _, err := a.heavyHostExec(env, 30*time.Minute, "apt-get", "install", "--yes", "mlnx-nfsrdma-dkms"); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("apt-get install mlnx-nfsrdma-dkms: %v (the package comes from the DOCA repo; check firmware.doca in the profile)", err)
		}
		did = append(did, "mlnx-nfsrdma-dkms installed")
	}
	if !confOK {
		if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("mkdir modules-load.d: %v", err)
		}
		if err := os.WriteFile(confPath, []byte(confWant), 0o644); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("write %s: %v", confPath, err)
		}
		did = append(did, "modules-load.d/nfsrdma.conf written")
	}
	if !loaded {
		for _, m := range []string{"rpcrdma", "xprtrdma", "svcrdma"} {
			_, _ = a.hostExec(env, 30*time.Second, "modprobe", m) // bash: || true
		}
		if a.moduleLoaded("rpcrdma") {
			did = append(did, "rpcrdma module loaded")
		} else {
			did = append(did, "WARNING rpcrdma not loaded (may need reboot)")
		}
	}
	return v1alpha1.StepDone, "NFSoRDMA configured: " + strings.Join(did, "; ")
}

// moduleLoaded asks the host lsmod for a module.
func (a *Agent) moduleLoaded(name string) bool {
	out, err := a.hostExecQuiet(nil, 30*time.Second, "lsmod")
	if err != nil {
		return false
	}
	for _, ln := range strings.Split(out, "\n") {
		if f := strings.Fields(ln); len(f) > 0 && f[0] == name {
			return true
		}
	}
	return false
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

// vfCountFor is the OS-level sriov_numvfs target: east-west count for rail
// NICs, north-south for DPUs, the profile value verbatim (0 = none).
// vfCountFor maps a function to its VF target. East-west VFs land ONLY on
// the rail-mapped functions (spec.rails; the bash applied NUMVF_EW to every
// ConnectX-class function, which would touch e.g. the bond0 uplink card —
// Kevin's correction, 2026-09-03); unmapped functions and DPUs without
// north-south VFs get none. DPUs take the north-south count.
func vfCountFor(profile *v1alpha1.NodePrepProfile, nic pciDevice) int {
	if nic.rail == "dpu" {
		return profile.Spec.NorthSouth.NumVFs
	}
	if nic.rail == "" {
		return 0
	}
	return profile.Spec.EastWest.NumVFs
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
