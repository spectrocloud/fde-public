package agent

import (
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
