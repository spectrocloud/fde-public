package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientfake "k8s.io/client-go/kubernetes/fake"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

func TestIsInstalledStatus(t *testing.T) {
	cases := map[string]bool{
		"install ok installed":       true,
		"hold ok installed":          true,
		"install ok installed\n":     true,
		"deinstall ok config-files":  false,
		"install ok half-configured": false,
		"":                           false,
		"garbage":                    false,
	}
	for in, want := range cases {
		if got := isInstalledStatus(in); got != want {
			t.Errorf("isInstalledStatus(%q) = %v, want %v", in, got, want)
		}
	}
}

// grubWantParams' VF gates (Kevin, dsx-new): pci=realloc joins the iommu
// parameters exactly when VFs are demanded — east-west, or north-south with
// the offloadEngine gate on. A stale northSouth.numVFs with DPU management
// off must not wake it (dpuNSVFs semantics, 0.1.84). ib_core.netns_mode is
// deliberately NOT under either gate — see
// TestStepAptPackagesNetnsModeIndependentOfVFs.
func TestGrubWantParamsVfGates(t *testing.T) {
	contains := func(list []string, s string) bool {
		for _, v := range list {
			if v == s {
				return true
			}
		}
		return false
	}

	// East-west VFs: iommu=pt and pci=realloc.
	p := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		EastWest: v1alpha1.EastWestSpec{NumVFs: 16},
	}}
	want := grubWantParams(p)
	if !contains(want, "pci=realloc") || !contains(want, "iommu=pt") {
		t.Fatalf("EW numVFs 16 must demand iommu=pt + pci=realloc, got %v", want)
	}

	// North-south VFs count only under the offloadEngine gate.
	yes := true
	p = &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		NorthSouth: v1alpha1.NorthSouthSpec{NumVFs: 8, OffloadEngine: true},
		Policy:     v1alpha1.PolicySpec{HostMutations: &yes},
	}}
	if want = grubWantParams(p); !contains(want, "pci=realloc") {
		t.Fatalf("NS numVFs 8 with offloadEngine must demand pci=realloc, got %v", want)
	}
	p = &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		NorthSouth: v1alpha1.NorthSouthSpec{NumVFs: 8},
	}}
	if want = grubWantParams(p); contains(want, "pci=realloc") || contains(want, "iommu=pt") {
		t.Fatalf("NS numVFs 8 without offloadEngine must demand no VF params, got %v", want)
	}
}

// The 0.1.93 check (Kevin): ib_core.netns_mode must be staged regardless of
// the VF count — a VF-less profile that carries rdmaNetnsMode still gets the
// modprobe.d file, the cmdline mirror, and (unlike a VF-carrying profile)
// no pci=realloc. Runs on the 0.1.95 CRD vocabulary ("exclusive") so the new
// words get full-step proof; the numeric legacy spellings keep their cells
// in TestStepAptPackagesGates.
func TestStepAptPackagesNetnsModeIndependentOfVFs(t *testing.T) {
	np := &v1alpha1.NodePrep{}
	etc := t.TempDir()
	a := &Agent{hostMutations: true, hostEtcDir: func() string { return etc },
		client: clientfake.NewSimpleClientset(),
		execFn: func(_ []string, _ time.Duration, name string, _ bool, _ []string) (string, error) {
			return "", nil
		}}
	yes := true
	// No numVFs anywhere — east-west zero, north-south zero and ungated.
	profile := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		HostBoot: v1alpha1.HostBootSpec{RDMANetnsMode: "exclusive"},
		Policy:   v1alpha1.PolicySpec{HostMutations: &yes},
	}}
	state, msg := stepAptPackages(a, np, profile)
	if state != v1alpha1.StepDone {
		t.Fatalf("VF-less netns-only profile: got %s %q, want Done", state, msg)
	}
	if got, _ := os.ReadFile(filepath.Join(etc, "modprobe.d/ib_core.conf")); string(got) != "options ib_core netns_mode=0\n" {
		t.Fatalf("modprobe.d must be staged at numVFs 0: %q", got)
	}
	dropin, err := os.ReadFile(filepath.Join(etc, "default/grub.d/90-nodeprep.cfg"))
	if err != nil {
		t.Fatalf("dropin must be written: %v", err)
	}
	if !strings.Contains(string(dropin), "ib_core.netns_mode=0") {
		t.Fatalf("the cmdline mirror must be staged at numVFs 0: %q", dropin)
	}
	if strings.Contains(string(dropin), "pci=realloc") {
		t.Fatalf("pci=realloc must not ride a VF-less profile: %q", dropin)
	}
	if len(a.pendingReboot) != 1 || a.pendingReboot[0].reason != v1alpha1.RebootGrubChanged {
		t.Fatalf("the staged mirror must still request its reboot, got %+v", a.pendingReboot)
	}
}

// The 0.1.95 CRD vocabulary (Kevin): spec.hostBoot.rdmaNetnsMode speaks
// exclusive | shared — the operator-facing words for the ib_core netns_mode
// kernel values 0 | 1 — and every staging surface (modprobe.d, cmdline
// mirror, ledger note) carries the numeric form. Stored profiles predating
// the CRD change keep their numeric spellings working byte-identically, and
// an unknown word fails the step loudly instead of staging an option the
// module would reject at load.
func TestStepAptPackagesNetnsModeVocabulary(t *testing.T) {
	np := &v1alpha1.NodePrep{}
	yes := true
	run := func(mode string) (v1alpha1.StepState, string) {
		etc := t.TempDir()
		a := &Agent{hostMutations: true, hostEtcDir: func() string { return etc },
			client: clientfake.NewSimpleClientset(),
			execFn: func(_ []string, _ time.Duration, name string, _ bool, _ []string) (string, error) {
				return "", nil
			}}
		profile := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
			HostBoot: v1alpha1.HostBootSpec{RDMANetnsMode: mode},
			Policy:   v1alpha1.PolicySpec{HostMutations: &yes},
		}}
		return stepAptPackages(a, np, profile)
	}

	state, msg := run("exclusive")
	if state != v1alpha1.StepDone || !strings.Contains(msg, "netns_mode=0 staged") {
		t.Fatalf("exclusive: got %s %q, want Done with the netns_mode=0 note", state, msg)
	}
	state, msg = run("shared")
	if state != v1alpha1.StepDone || !strings.Contains(msg, "netns_mode=1 staged") {
		t.Fatalf("shared: got %s %q, want Done with the netns_mode=1 note", state, msg)
	}
	state, msg = run("0") // legacy numeric spelling of a pre-0.1.95 stored profile
	if state != v1alpha1.StepDone || !strings.Contains(msg, "netns_mode=0 staged") {
		t.Fatalf("legacy \"0\": got %s %q, want Done with the netns_mode=0 note", state, msg)
	}
	state, msg = run("always")
	if state != v1alpha1.StepFailed || !strings.Contains(msg, "unsupported rdmaNetnsMode") {
		t.Fatalf("unknown word: got %s %q, want Failed with the vocabulary hint", state, msg)
	}
}

// stepAptPackages decision gates: no configuration skips, detect-only mode
// blocks, a configured deb missing from the cache fails loudly, and the
// folded ib_core staging (0.1.75) runs even with no packages configured —
// but never without mutations.
func TestStepAptPackagesGates(t *testing.T) {
	np := &v1alpha1.NodePrep{}
	a := &Agent{} // detect-only by zero value

	state, msg := stepAptPackages(a, np, &v1alpha1.NodePrepProfile{})
	if state != v1alpha1.StepDone || !strings.Contains(msg, "skipped") {
		t.Fatalf("empty config: got %s %q, want Done/skipped", state, msg)
	}

	profile := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		Firmware: v1alpha1.FirmwareSource{DOCA: v1alpha1.DOCASource{Packages: []string{"doca-ofed"}}},
	}}
	state, msg = stepAptPackages(a, np, profile)
	if state != v1alpha1.StepBlocked || !strings.Contains(msg, "-host-mutations") {
		t.Fatalf("detect-only: got %s %q, want Blocked", state, msg)
	}

	yes := true
	a2 := &Agent{hostMutations: true, spcxDir: func() string { return t.TempDir() }}
	profile2 := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		Firmware: v1alpha1.FirmwareSource{DOCA: v1alpha1.DOCASource{Deb: "doca-host.deb"}},
		Policy:   v1alpha1.PolicySpec{HostMutations: &yes},
	}}
	state, msg = stepAptPackages(a2, np, profile2)
	if state != v1alpha1.StepFailed || !strings.Contains(msg, "missing") {
		t.Fatalf("deb missing: got %s %q, want Failed/missing", state, msg)
	}

	// Profile-only node (rdmaNetnsMode set, no DOCA configured): the folded
	// staging still writes modprobe.d — this is the case the standalone
	// ibCoreNetns step used to cover. Detect-only blocks on it. The stub
	// execFn absorbs the update-initramfs refresh (no nsenter off-host).
	etcd := t.TempDir()
	a3 := &Agent{hostMutations: true, hostEtcDir: func() string { return etcd },
		client: clientfake.NewSimpleClientset(), // the reboot request emits an event
		execFn: func(_ []string, _ time.Duration, _ string, _ bool, _ []string) (string, error) {
			return "", nil
		}}
	profile3 := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		HostBoot: v1alpha1.HostBootSpec{RDMANetnsMode: "0"},
		Policy:   v1alpha1.PolicySpec{HostMutations: &yes},
	}}
	state, msg = stepAptPackages(&Agent{hostEtcDir: func() string { return t.TempDir() }}, np, profile3)
	if state != v1alpha1.StepBlocked || !strings.Contains(msg, "-host-mutations") {
		t.Fatalf("profile-only, detect-only: got %s %q, want Blocked", state, msg)
	}
	state, msg = stepAptPackages(a3, np, profile3)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "netns_mode=0 staged") || !strings.Contains(msg, "no DOCA packages configured") {
		t.Fatalf("profile-only, mutations on: got %s %q, want Done with the staged note", state, msg)
	}
	if got, _ := os.ReadFile(filepath.Join(etcd, "modprobe.d/ib_core.conf")); string(got) != "options ib_core netns_mode=0\n" {
		t.Fatalf("profile-only staging wrote wrong content: %q", got)
	}
	// 0.1.90 (Kevin, dsx-nwo-267 reboot loop): the SR-IOV Network Operator
	// checks ONLY the ib_core.netns_mode kernel parameter, so the staging
	// must also mirror the mode into the grub dropin.
	if dropin, err := os.ReadFile(filepath.Join(etcd, "default/grub.d/90-nodeprep.cfg")); err != nil || !strings.Contains(string(dropin), "ib_core.netns_mode=0") {
		t.Fatalf("profile-only staging must mirror netns_mode on the cmdline dropin (err=%v): %q", err, dropin)
	}
	// The staged mode must not wait for someone else's reboot: an ib_core-only
	// staging requests its own (quiet-checkpoint) reboot — 0.1.75 dropped the
	// standalone step's request here, and the module never reloaded. 0.1.90
	// routes the request through the mirrored cmdline param, so the reason is
	// GrubChanged with the same quiet form.
	if len(a3.pendingReboot) != 1 || a3.pendingReboot[0].reason != v1alpha1.RebootGrubChanged || a3.pendingReboot[0].haltNow {
		t.Fatalf("profile-only staging must request its reboot via the mirrored cmdline param, got %+v", a3.pendingReboot)
	}
}

// The folded grub staging (0.1.78): the dropin + update-grub run inside
// aptPackages and the cmdline reboot rides the same decision as the DOCA
// install — the standalone step ran after the DOCA halt and armed its own
// extra boot.
func TestStepAptPackagesGrubFold(t *testing.T) {
	np := &v1alpha1.NodePrep{}
	etc := t.TempDir()
	var calls []string
	a := &Agent{hostMutations: true, hostEtcDir: func() string { return etc },
		client: clientfake.NewSimpleClientset(),
		execFn: func(_ []string, _ time.Duration, name string, _ bool, _ []string) (string, error) {
			calls = append(calls, name)
			if name == "dpkg-query" {
				return "install ok installed", nil
			}
			return "", nil
		}}
	yes := true
	profile := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		// Packages-only DOCA (no deb): all installed by the stub, so the
		// pass has no package work — the grub change must still stage and
		// request its reboot.
		Firmware: v1alpha1.FirmwareSource{DOCA: v1alpha1.DOCASource{Packages: []string{"doca-all"}}},
		HostBoot: v1alpha1.HostBootSpec{IOMMU: "intel", Hugepages: v1alpha1.HugepagesSpec{Pages2M: 5120}},
		Policy:   v1alpha1.PolicySpec{HostMutations: &yes},
	}}
	state, msg := stepAptPackages(a, np, profile)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "grub 90-nodeprep.cfg written") {
		t.Fatalf("grub fold: got %s %q, want Done with the grub note", state, msg)
	}
	if !strings.Contains(msg, "DOCA packages already installed") {
		t.Fatalf("clean dpkg state must still be reported: %q", msg)
	}
	dropin, err := os.ReadFile(filepath.Join(etc, "default/grub.d/90-nodeprep.cfg"))
	if err != nil || !strings.Contains(string(dropin), "intel_iommu=on") || !strings.Contains(string(dropin), "hugepages=5120") {
		t.Fatalf("dropin wrong (err=%v): %q", err, dropin)
	}
	var updateGrub bool
	for _, c := range calls {
		if c == "update-grub" {
			updateGrub = true
		}
	}
	if !updateGrub {
		t.Fatalf("update-grub must run: %v", calls)
	}
	// Packages clean → the grub reboot is the quiet-checkpoint form, not a
	// DOCA halt.
	if len(a.pendingReboot) != 1 || a.pendingReboot[0].reason != v1alpha1.RebootGrubChanged || a.pendingReboot[0].haltNow {
		t.Fatalf("grub-only change must request the quiet GrubChanged reboot, got %+v", a.pendingReboot)
	}

	// An agent restart loses the in-memory request: the dropin is already
	// arranged but the kernel has not booted with the parameters — the next
	// pass must re-request the reboot (without rewriting the dropin).
	a2 := &Agent{hostMutations: true, hostEtcDir: func() string { return etc },
		client: clientfake.NewSimpleClientset(),
		execFn: func(_ []string, _ time.Duration, name string, _ bool, _ []string) (string, error) {
			calls = append(calls, "a2:"+name)
			if name == "dpkg-query" {
				return "install ok installed", nil
			}
			return "", nil
		}}
	state, msg = stepAptPackages(a2, np, profile)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "already arranged") {
		t.Fatalf("arranged-but-not-running: got %s %q, want Done reporting the arrangement", state, msg)
	}
	if len(a2.pendingReboot) != 1 || a2.pendingReboot[0].reason != v1alpha1.RebootGrubChanged {
		t.Fatalf("arranged-but-not-running must re-request the reboot, got %+v", a2.pendingReboot)
	}
	for _, c := range calls {
		if c == "a2:update-grub" {
			t.Fatalf("an already-correct dropin must not be rewritten: %v", calls)
		}
	}
}

// The 0.1.90 ib_core → cmdline mirror (Kevin, dsx-nwo-267): the SR-IOV
// Network Operator checks ONLY the ib_core.netns_mode kernel parameter — a
// modprobe.d-only netns_mode read as unset, the operator's own attempt to
// set the parameter failed on Ubuntu, and the node reboot-looped. Kevin's
// manual fix was the nodeprep dropin's pairs line; the mirror reproduces
// exactly that through the standard idempotent machinery, alongside the
// profile's other managed parameters. Pinned: the dropin carries the
// mirrored param alongside the profile's own params, the quiet GrubChanged
// reboot is requested, an arranged-but-not-running pass re-requests without
// rewriting, and once the kernel has booted with the param the step goes
// quiet — no update-grub, no initramfs refresh, no reboot of our own.
func TestStepAptPackagesIbCoreGrubMirror(t *testing.T) {
	np := &v1alpha1.NodePrep{}
	etc := t.TempDir()
	var calls []string
	newAgent := func() *Agent {
		return &Agent{hostMutations: true, hostEtcDir: func() string { return etc },
			client: clientfake.NewSimpleClientset(),
			execFn: func(_ []string, _ time.Duration, name string, _ bool, _ []string) (string, error) {
				calls = append(calls, name)
				return "", nil
			}}
	}
	yes := true
	// The live basic-profile shape: hugepages on hostBoot plus rdmaNetnsMode,
	// the combination that left modprobe.d carrying netns_mode=0 while the
	// cmdline did not.
	profile := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		HostBoot: v1alpha1.HostBootSpec{
			RDMANetnsMode: "0",
			Hugepages:     v1alpha1.HugepagesSpec{Pages2M: 5120},
		},
		Policy: v1alpha1.PolicySpec{HostMutations: &yes},
	}}

	// Fresh pass: modprobe.d staged, dropin carries the profile params plus
	// the mirror, update-grub runs, one quiet GrubChanged reboot.
	calls = nil
	a := newAgent()
	state, msg := stepAptPackages(a, np, profile)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "grub 90-nodeprep.cfg written") {
		t.Fatalf("fresh mirror pass: got %s %q, want Done with the grub note", state, msg)
	}
	if got, _ := os.ReadFile(filepath.Join(etc, "modprobe.d/ib_core.conf")); string(got) != "options ib_core netns_mode=0\n" {
		t.Fatalf("modprobe.d content wrong: %q", got)
	}
	dropin, err := os.ReadFile(filepath.Join(etc, "default/grub.d/90-nodeprep.cfg"))
	if err != nil {
		t.Fatalf("dropin must be written: %v", err)
	}
	if !strings.Contains(string(dropin), "hugepages=5120 ib_core.netns_mode=0") &&
		!strings.Contains(string(dropin), "ib_core.netns_mode=0 hugepages=5120") {
		t.Fatalf("the dropin pairs line must carry the mirror alongside the profile's params: %q", dropin)
	}
	if !strings.Contains(string(dropin), "ib_core.netns_mode=") || !strings.Contains(strings.Split(string(dropin), "\n")[1], "ib_core") {
		t.Fatalf("the sed strip must clear the mirrored key before re-appending: %q", dropin)
	}
	if len(a.pendingReboot) != 1 || a.pendingReboot[0].reason != v1alpha1.RebootGrubChanged || a.pendingReboot[0].haltNow {
		t.Fatalf("the mirror must ride the quiet GrubChanged reboot, got %+v", a.pendingReboot)
	}

	// Arranged but not running (an agent restart lost the request): re-request
	// without rewriting the dropin.
	calls = nil
	a2 := newAgent()
	state, msg = stepAptPackages(a2, np, profile)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "already arranged") {
		t.Fatalf("arranged-but-not-running: got %s %q, want Done reporting the arrangement", state, msg)
	}
	if len(a2.pendingReboot) != 1 || a2.pendingReboot[0].reason != v1alpha1.RebootGrubChanged {
		t.Fatalf("arranged-but-not-running must re-request the reboot, got %+v", a2.pendingReboot)
	}
	for _, c := range calls {
		if c == "update-grub" {
			t.Fatalf("an arranged dropin must not be rewritten: %v", calls)
		}
	}

	// Post-boot quiet pass: the kernel booted with the mirror and the module
	// holds the staged value — no staging, no rewrite, no reboot of our own.
	cmdline := t.TempDir() + "/cmdline"
	os.WriteFile(cmdline, []byte("ro net.ifnames=4 intel_iommu=on iommu=pt default_hugepagesz=2M hugepagesz=2M hugepages=5120 ib_core.netns_mode=0 quiet\n"), 0o644)
	oldCmd, oldSys := grubCmdlineFile, ibCoreNetnsSysfs
	grubCmdlineFile, ibCoreNetnsSysfs = cmdline, filepath.Join(t.TempDir(), "netns_mode")
	defer func() { grubCmdlineFile, ibCoreNetnsSysfs = oldCmd, oldSys }()
	os.WriteFile(ibCoreNetnsSysfs, []byte("N\n"), 0o644) // netns_mode=0 in the running module
	calls = nil
	a3 := newAgent()
	state, msg = stepAptPackages(a3, np, profile)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "kernel cmdline has required parameters") {
		t.Fatalf("post-boot pass: got %s %q, want Done with the quiet cmdline note", state, msg)
	}
	if len(a3.pendingReboot) != 0 {
		t.Fatalf("a booted mirror must not re-request the reboot, got %+v", a3.pendingReboot)
	}
	for _, c := range calls {
		if c == "update-grub" || c == "update-initramfs" {
			t.Fatalf("the post-boot pass must be quiet: %v", calls)
		}
	}
}

// The folded ib_core netns staging: write-once, per-process dedup for the
// initramfs refresh, and the running-module check via the sysfs seam. The
// mode arrives canonical (netnsModeCanonical resolved it), so the vocabulary
// cells here double as the mapping proof: exclusive→0, shared→1.
func TestStageIbCoreNetns(t *testing.T) {
	etc := t.TempDir()
	sys := filepath.Join(t.TempDir(), "netns_mode")
	a := &Agent{hostEtcDir: func() string { return etc }}

	if refresh, err := a.stageIbCoreNetns(""); refresh || err != nil {
		t.Fatalf("no mode set: got refresh=%v err=%v, want false/nil", refresh, err)
	}
	if _, err := os.Stat(filepath.Join(etc, "modprobe.d/ib_core.conf")); !os.IsNotExist(err) {
		t.Fatalf("no mode set must not write modprobe.d")
	}

	refresh, err := a.stageIbCoreNetns("0")
	if err != nil || !refresh {
		t.Fatalf("fresh staging: got refresh=%v err=%v, want true/nil", refresh, err)
	}
	if got, _ := os.ReadFile(filepath.Join(etc, "modprobe.d/ib_core.conf")); string(got) != "options ib_core netns_mode=0\n" {
		t.Fatalf("staged content wrong: %q", got)
	}
	// Same process, flag armed: no second refresh for unchanged content.
	if refresh, _ := a.stageIbCoreNetns("0"); refresh {
		t.Fatalf("armed process must not re-request the refresh")
	}

	// Fresh process (a2): file already staged, running module already
	// matches the profile → nothing pending.
	os.WriteFile(sys, []byte("N\n"), 0o644)
	ibCoreNetnsSysfs = sys
	t.Cleanup(func() { ibCoreNetnsSysfs = "/sys/module/ib_core/parameters/netns_mode" })
	a2 := &Agent{hostEtcDir: func() string { return etc }}
	if refresh, _ := a2.stageIbCoreNetns("0"); refresh {
		t.Fatalf("running module matches the staged mode: no refresh pending")
	}

	// Fresh process (a3): staged but the running module holds the other
	// value → the crashed-pass refresh must re-arm.
	os.WriteFile(sys, []byte("Y\n"), 0o644)
	a3 := &Agent{hostEtcDir: func() string { return etc }}
	if refresh, _ := a3.stageIbCoreNetns("0"); !refresh {
		t.Fatalf("staged but not loaded: refresh must be pending (retries a crashed pass)")
	}

	// 0.1.95 vocabulary (Kevin): the CRD words stage the kernel's numeric
	// form — byte-identical to the numeric spelling, so re-wording a stored
	// profile across the CRD change re-stages nothing. "exclusive" must
	// leave the file from the "0" staging above untouched.
	a4 := &Agent{hostEtcDir: func() string { return etc }}
	if _, err := a4.stageIbCoreNetns("exclusive"); err != nil {
		t.Fatalf("exclusive: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(etc, "modprobe.d/ib_core.conf")); string(got) != "options ib_core netns_mode=0\n" {
		t.Fatalf("exclusive must stage netns_mode=0 (byte-identical to \"0\"): %q", got)
	}
	etc5 := t.TempDir()
	a5 := &Agent{hostEtcDir: func() string { return etc5 }}
	if _, err := a5.stageIbCoreNetns("shared"); err != nil {
		t.Fatalf("shared: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(etc5, "modprobe.d/ib_core.conf")); string(got) != "options ib_core netns_mode=1\n" {
		t.Fatalf("shared must stage netns_mode=1: %q", got)
	}
	if _, err := a5.stageIbCoreNetns("always"); err == nil || !strings.Contains(err.Error(), "unsupported rdmaNetnsMode") {
		t.Fatalf("an unknown mode must fail loudly instead of staging it, got %v", err)
	}
}
