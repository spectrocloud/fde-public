package agent

import (
	"strings"
	"testing"

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
// blocks, and a configured deb missing from the cache fails loudly.
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
}
