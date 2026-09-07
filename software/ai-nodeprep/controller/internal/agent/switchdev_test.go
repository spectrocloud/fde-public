package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// The switchdev wiring (0.1.68): fn_set_vfs's eswitch flip gates, the
// devlink mode parser, and the two OVS steps' skip gates. The OVS exec
// bodies are validated live on DSX Air — the unit environment has no host
// tooling — so these tests pin the gates and the parsing, the parts that
// silently mis-gate.

func TestParseEswitchMode(t *testing.T) {
	cases := []struct {
		out, want string
	}{
		{"pci/0000:05:00.0: mode legacy\n", "legacy"},
		{"pci/0000:05:00.0: mode switchdev\n", "switchdev"},
		// Newer iproute2 appends the other eswitch parameters.
		{"pci/0000:05:00.0: mode legacy inline-mode none encap-mode none\n", "legacy"},
		{"pci/0000:05:00.0: mode switchdev inline-mode none encap-mode basic\n", "switchdev"},
		{"", ""},
		{"pci/0000:05:00.0: (device gone)\n", ""},
	}
	for _, c := range cases {
		if got := parseEswitchMode(c.out); got != c.want {
			t.Fatalf("parseEswitchMode(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

// The bash gate (fn_set_vfs L625): ConnectX/SuperNIC class + east-west VF
// demand + switchdev mode. The controller's VF demand is rail-mapped, so
// unmapped functions and DPUs never flip here — a DPU's demand comes from
// the north-south side and bash treats that side as legacy.
func TestSwitchdevFlipGate(t *testing.T) {
	profile := &v1alpha1.NodePrepProfile{}
	profile.Spec.EastWest.EswitchMode = "switchdev"
	profile.Spec.EastWest.NumVFs = 1
	rail := pciDevice{pci: "0000:05:00.0", devType: "ConnectX7", rail: "r0"}
	if !switchdevFlip(profile, rail) {
		t.Fatal("a rail-mapped ConnectX-7 with a VF demand under switchdev must flip")
	}
	dpu := pciDevice{pci: "0000:0d:00.0", devType: "ConnectX7", rail: "dpu"}
	if switchdevFlip(profile, dpu) {
		t.Fatal("a DPU's north-south VFs must not flip (bash treats the NS side as legacy)")
	}
	unmapped := pciDevice{pci: "0000:0e:00.0", devType: "ConnectX7"}
	if switchdevFlip(profile, unmapped) {
		t.Fatal("an unmapped function has no VF demand and must not flip")
	}
	legacyProfile := &v1alpha1.NodePrepProfile{}
	legacyProfile.Spec.EastWest.EswitchMode = "legacy"
	legacyProfile.Spec.EastWest.NumVFs = 1
	if switchdevFlip(legacyProfile, rail) {
		t.Fatal("a legacy profile must not flip")
	}
	noVFs := &v1alpha1.NodePrepProfile{}
	noVFs.Spec.EastWest.EswitchMode = "switchdev"
	if switchdevFlip(noVFs, rail) {
		t.Fatal("no VF demand must not flip")
	}
}

func TestOvsValueTrim(t *testing.T) {
	cases := []struct{ in, want string }{
		{"true\n", "true"},
		{"\"300000\"\n", "300000"},
		{"  secure  \n", "secure"},
		{"\"netdev\"\n", "netdev"},
		{"\n", ""},
	}
	for _, c := range cases {
		if got := ovsValue(c.in); got != c.want {
			t.Fatalf("ovsValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The convergence contract: ovsSetupConverged returns "" only when
// converged, and stepOvsSetup must treat a divergence as work to do — never
// Done. Found live on DSX Air (0.1.68): the call sites read the inverted
// contract, so a divergent host ("OVS is not answering ovs-vsctl",
// "other_config:doca-init = no key, want true") reported StepDone and the
// apply path never ran; ovsBridges then failed forever against bridges
// ovsSetup was supposed to create.
func TestOvsSetupDivergenceMustNotReadDone(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usr", "bin", "ovs-vsctl"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldRoot := hostToolRoot
	hostToolRoot = dir
	defer func() { hostToolRoot = oldRoot }()

	a := &Agent{mellanoxFns: []pciDevice{{pci: "0000:05:00.0", devType: "ConnectX7", rail: "r0", netdev: "eth_r0"}}}
	np := &v1alpha1.NodePrep{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	profile := &v1alpha1.NodePrepProfile{}
	profile.Spec.EastWest.EswitchMode = "switchdev"
	profile.Spec.EastWest.NumVFs = 1

	// A bare agent cannot exec the host: OVS is not answering — divergent.
	// The step must fall through to the mutations gate (Blocked on a bare
	// agent), never return Done with the divergence as its message.
	st, msg := stepOvsSetup(a, np, profile)
	if st == v1alpha1.StepDone {
		t.Fatalf("divergent OVS state must not read Done, got Done %q", msg)
	}
	if st != v1alpha1.StepBlocked || !strings.Contains(msg, "-host-mutations") {
		t.Fatalf("divergent detect with mutations off must Block, got %s %q", st, msg)
	}

	// The same divergence behind the mutations gate: with mutations on the
	// step must attempt the apply (which fails hostExec on a bare agent —
	// Failed, still never Done).
	a.hostMutations = true
	yes := true
	profile.Spec.Policy.HostMutations = &yes
	if st, msg := stepOvsSetup(a, np, profile); st == v1alpha1.StepDone {
		t.Fatalf("a failed apply must not read Done, got Done %q", msg)
	}
}

// The OVS steps' skip gates: without an inventory (bare Agent) every gate
// below the skip returns before any exec — a lab node without Mellanox
// must walk to Ready with these steps Done, exactly like the bash's
// switchdev blocks which only fire on the rail inventory.
func TestOvsStepsSkipGates(t *testing.T) {
	a := &Agent{}
	np := &v1alpha1.NodePrep{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	legacy := &v1alpha1.NodePrepProfile{}
	legacy.Spec.EastWest.EswitchMode = "legacy"
	legacy.Spec.EastWest.NumVFs = 1

	if st, msg := stepOvsSetup(a, np, legacy); st != v1alpha1.StepDone || !strings.Contains(msg, "eswitch mode is not switchdev") {
		t.Fatalf("legacy profile must skip ovsSetup, got %s %q", st, msg)
	}
	if st, msg := stepOvsBridges(a, np, legacy); st != v1alpha1.StepDone || !strings.Contains(msg, "eswitch mode is not switchdev") {
		t.Fatalf("legacy profile must skip ovsBridges, got %s %q", st, msg)
	}

	switchdev := &v1alpha1.NodePrepProfile{}
	switchdev.Spec.EastWest.EswitchMode = "switchdev"
	switchdev.Spec.EastWest.NumVFs = 1
	if st, msg := stepOvsSetup(a, np, switchdev); st != v1alpha1.StepDone || !strings.Contains(msg, "no Mellanox hardware") {
		t.Fatalf("switchdev without Mellanox must skip ovsSetup, got %s %q", st, msg)
	}
	if st, msg := stepOvsBridges(a, np, switchdev); st != v1alpha1.StepDone || !strings.Contains(msg, "no Mellanox hardware") {
		t.Fatalf("switchdev without Mellanox must skip ovsBridges, got %s %q", st, msg)
	}

	// The fn_set_vfs any-VF-demand gate: switchdev with no VFs requested
	// skips the OVS setup even on Mellanox hardware.
	switchdev.Spec.EastWest.NumVFs = 0
	inventory := &Agent{mellanoxFns: []pciDevice{{pci: "0000:05:00.0", devType: "ConnectX7", rail: "r0", netdev: "eth_r0"}}}
	if st, msg := stepOvsSetup(inventory, np, switchdev); st != v1alpha1.StepDone || !strings.Contains(msg, "no VFs requested") {
		t.Fatalf("switchdev without VFs must skip ovsSetup (fn_set_vfs gate), got %s %q", st, msg)
	}
	// ovsBridges has no such gate in the bash (fn_add_pfs_to_rail_bridges
	// fires on the mode alone) — with OVS tooling absent it Blocks instead.
	if st, msg := stepOvsBridges(inventory, np, switchdev); st != v1alpha1.StepBlocked || !strings.Contains(msg, "ovs-vsctl not found") {
		t.Fatalf("switchdev ovsBridges without OVS tooling must Block, got %s %q", st, msg)
	}
}

// The rail-key grammar must not flip when VFs appear: a VF enumerates under
// its PF's bus:device (0000:05:00.2 → fn 05:00), and counting it in the
// multi-function check keyed every rail r0_p0 the moment numvfs>0 — the
// OVS/udev steps then re-applied against eth_r0_p0/br-rail-r0_p0, which do
// not exist (found live on DSX Air, 0.1.70: pre-reboot keys r0_p0, post-
// reboot keys r0, both "correct" for their boot's inventory). The bash
// scans PFs only (mst status), so the count is PFs only.
func TestAssignRailsIgnoresVFs(t *testing.T) {
	rails := map[string]string{"05:00": "r0", "06:00": "r1"}
	fns := []pciDevice{
		{pci: "0000:05:00.0", fn: "05:00"},
		{pci: "0000:05:00.2", fn: "05:00", isVF: true},
		{pci: "0000:06:00.0", fn: "06:00"},
		{pci: "0000:06:00.1", fn: "06:00"},
	}
	assignRails(fns, rails)
	want := []string{"r0", "r0", "r1_p0", "r1_p1"}
	for i, w := range want {
		if fns[i].rail != w {
			t.Fatalf("fn %s: rail = %q, want %q", fns[i].pci, fns[i].rail, w)
		}
	}
}
