package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		execFn: func(_ []string, _ time.Duration, _ string, _ bool, _ []string) (string, error) {
			return "", nil
		}}
	profile3 := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		HostBoot: v1alpha1.HostBootSpec{RDMANetnsMode: "0"},
		Policy:   v1alpha1.PolicySpec{HostMutations: &yes},
	}}
	state, msg = stepAptPackages(&Agent{}, np, profile3)
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
}

// The folded ib_core netns staging: write-once, per-process dedup for the
// initramfs refresh, and the running-module check via the sysfs seam.
func TestStageIbCoreNetns(t *testing.T) {
	etc := t.TempDir()
	sys := filepath.Join(t.TempDir(), "netns_mode")
	a := &Agent{hostEtcDir: func() string { return etc }}
	p := &v1alpha1.NodePrepProfile{}

	if refresh, err := a.stageIbCoreNetns(p); refresh || err != nil {
		t.Fatalf("no mode set: got refresh=%v err=%v, want false/nil", refresh, err)
	}
	if _, err := os.Stat(filepath.Join(etc, "modprobe.d/ib_core.conf")); !os.IsNotExist(err) {
		t.Fatalf("no mode set must not write modprobe.d")
	}

	p.Spec.HostBoot.RDMANetnsMode = "0"
	refresh, err := a.stageIbCoreNetns(p)
	if err != nil || !refresh {
		t.Fatalf("fresh staging: got refresh=%v err=%v, want true/nil", refresh, err)
	}
	if got, _ := os.ReadFile(filepath.Join(etc, "modprobe.d/ib_core.conf")); string(got) != "options ib_core netns_mode=0\n" {
		t.Fatalf("staged content wrong: %q", got)
	}
	// Same process, flag armed: no second refresh for unchanged content.
	if refresh, _ := a.stageIbCoreNetns(p); refresh {
		t.Fatalf("armed process must not re-request the refresh")
	}

	// Fresh process (a2): file already staged, running module already
	// matches the profile → nothing pending.
	os.WriteFile(sys, []byte("N\n"), 0o644)
	ibCoreNetnsSysfs = sys
	t.Cleanup(func() { ibCoreNetnsSysfs = "/sys/module/ib_core/parameters/netns_mode" })
	a2 := &Agent{hostEtcDir: func() string { return etc }}
	if refresh, _ := a2.stageIbCoreNetns(p); refresh {
		t.Fatalf("running module matches the staged mode: no refresh pending")
	}

	// Fresh process (a3): staged but the running module holds the other
	// value → the crashed-pass refresh must re-arm.
	os.WriteFile(sys, []byte("Y\n"), 0o644)
	a3 := &Agent{hostEtcDir: func() string { return etc }}
	if refresh, _ := a3.stageIbCoreNetns(p); !refresh {
		t.Fatalf("staged but not loaded: refresh must be pending (retries a crashed pass)")
	}
}
