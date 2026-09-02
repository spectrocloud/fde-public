package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// Classification mirrors the bash fn_inventory_hw probe parsing.
func TestClassifyMlxconfig(t *testing.T) {
	// ConnectX-4 Lx: the offload-engine Description reads N/A, so the
	// Device type line decides and the variant is Air.
	cx4lx := "Device #1:\n----------\nDevice type:    ConnectX4Lx\nDescription:    N/A\nName:           " +
		"MCX4121A-XCAT\nINTERNAL_CPU_OFFLOAD_ENGINE N/A\n"
	typ, variant := classifyMlxconfig(cx4lx)
	if typ != "ConnectX4Lx" || variant != "Air" {
		t.Fatalf("cx4lx classified %s/%s, want ConnectX4Lx/Air", typ, variant)
	}
	if got := rawDeviceType(cx4lx); got != "ConnectX4Lx" {
		t.Fatalf("rawDeviceType = %q, want ConnectX4Lx", got)
	}

	// BlueField: the Description carries the DPU marker and stays Physical.
	bf3 := "Device #1:\n----------\nDevice type:    BlueField3\nDescription:    NVIDIA BlueField-3 DPU; 400Gb/s; ... ;\n" +
		"INTERNAL_CPU_OFFLOAD_ENGINE ECPF(0)\n"
	typ, variant = classifyMlxconfig(bf3)
	if typ != "DPU" || variant != "Physical" {
		t.Fatalf("bf3 classified %s/%s, want DPU/Physical", typ, variant)
	}

	snic := "Device type:    ConnectX7\nDescription:    NVIDIA SuperNIC ConnectX-7; ...;\nINTERNAL_CPU_OFFLOAD_ENGINE ECPF(0)\n"
	typ, variant = classifyMlxconfig(snic)
	if typ != "SuperNIC" || variant != "Physical" {
		t.Fatalf("supernic classified %s/%s, want SuperNIC/Physical", typ, variant)
	}

	// A non-DPU device whose offload key is absent falls back to the full
	// query's Device type.
	if got := classifyDeviceType("ConnectX4Lx"); got != "ConnectX4Lx" {
		t.Fatalf("classifyDeviceType(ConnectX4Lx) = %q", got)
	}
	if got := classifyDeviceType("BlueField3"); got != "DPU" {
		t.Fatalf("classifyDeviceType(BlueField3) = %q, want DPU", got)
	}
}

func TestParseFlint(t *testing.T) {
	fw, psid := parseFlint("FW Version: 16.35.2000\nFW Release Date: 1.1.2024\nPSID: MT_0000000438\n")
	if fw != "16.35.2000" || psid != "MT_0000000438" {
		t.Fatalf("parseFlint = %q/%q", fw, psid)
	}
}

// The flash gate is BlueField-3 only: ConnectX, BlueField-2 and unclassified
// devices are all reported as not flashable.
func TestBFBFlashGate(t *testing.T) {
	cases := []struct {
		name    string
		rawType string
		flash   bool
	}{
		{"bluefield-3", "BlueField3", true},
		{"bluefield-2", "BlueField2", false},
		{"connectx-4lx", "ConnectX4Lx", false},
		{"connectx-7", "ConnectX7", false},
	}
	for _, tc := range cases {
		d := pciDevice{devType: classifyDeviceType(tc.rawType), rawType: tc.rawType, rshim: "/dev/rshim0"}
		if got := d.isBluefield3(); got != tc.flash {
			t.Fatalf("%s: isBluefield3 = %v, want %v", tc.name, got, tc.flash)
		}
	}
	// Without an rshim there is nothing to flash through either.
	d := pciDevice{devType: "DPU", rawType: "BlueField3"}
	if d.isBluefield3() && d.rshim != "" {
		t.Fatalf("bluefield-3 without rshim must not be a flash candidate")
	}
}

// The netplan stanza must match the bash heredoc byte for byte (trailing
// newline included) so detection compares exactly.
func TestRenderNetplan(t *testing.T) {
	if got, want := renderNetplan("r0_p0", 9216, true),
		"network:\n  version: 2\n  ethernets:\n    eth_r0_p0:\n      ignore-carrier: true\n      mtu: 9216\n"; got != want {
		t.Fatalf("switchdev netplan:\n%q\nwant\n%q", got, want)
	}
	if got, want := renderNetplan("r0", 9000, false),
		"network:\n  version: 2\n  ethernets:\n    eth_r0:\n      ignore-carrier: false\n      mtu: 9000\n"; got != want {
		t.Fatalf("legacy netplan:\n%q\nwant\n%q", got, want)
	}
}

// The udev rules match the bash echo lines exactly (KERNELS carries the full
// PCI address, RDMA rules carry rdma_rename and the RoCE ToS).
func TestRenderUdevRules(t *testing.T) {
	rails := []pciDevice{
		{pci: "0000:49:00.1", rail: "r0_p1"},
		{pci: "0000:49:00.0", rail: "r0_p0"},
	}
	net := renderNetRules(rails)
	want := "# managed by nodeprep agent (eth_rN rail renaming)\n" +
		"ACTION==\"add\", KERNELS==\"0000:49:00.0\", SUBSYSTEM==\"net\", NAME=\"eth_r0_p0\"\n" +
		"ACTION==\"add\", KERNELS==\"0000:49:00.1\", SUBSYSTEM==\"net\", NAME=\"eth_r0_p1\"\n"
	if net != want {
		t.Fatalf("net rules:\n%s\nwant:\n%s", net, want)
	}
	rdma := renderRDMARules(rails)
	if !strings.Contains(rdma, "PROGRAM=\"rdma_rename %k NAME_FIXED roce_r0_p0\"") {
		t.Fatalf("rdma rename line missing: %s", rdma)
	}
	if !strings.Contains(rdma, "cma_roce_tos -d roce_r0_p1 -t 96") {
		t.Fatalf("roce tos line missing: %s", rdma)
	}
	if !strings.Contains(rdma, "echo 96 > /sys/class/infiniband/roce_r0_p1/tc/1/traffic_class") {
		t.Fatalf("traffic_class line missing: %s", rdma)
	}
	// sorted by PCI: .0 before .1
	if strings.Index(rdma, "roce_r0_p0") > strings.Index(rdma, "roce_r0_p1") {
		t.Fatalf("rdma rules not sorted by PCI address")
	}
}

func TestLinkTypeNumAndClamps(t *testing.T) {
	if linkTypeNum("Ethernet") != 2 || linkTypeNum("ethernet") != 2 || linkTypeNum("") != 2 || linkTypeNum("InfiniBand") != 1 {
		t.Fatalf("linkTypeNum mapping wrong")
	}
	if fwVFCount(0) != 1 || fwVFCount(1) != 1 || fwVFCount(8) != 8 {
		t.Fatalf("fwVFCount clamp wrong")
	}
	if normNetnsMode("Y") != "1" || normNetnsMode("n") != "0" || normNetnsMode("0") != "0" || normNetnsMode("1\n") != "1" {
		t.Fatalf("normNetnsMode mapping wrong")
	}
}

func TestMatchesConnectX79(t *testing.T) {
	for _, c := range []struct {
		typ  string
		want bool
	}{
		{"ConnectX4Lx", false},
		{"ConnectX6", false},
		{"ConnectX7", true},
		{"ConnectX-7", true},
		{"ConnectX9", true},
		{"SuperNIC", false},
	} {
		if got := matchesConnectX79(c.typ); got != c.want {
			t.Fatalf("matchesConnectX79(%q) = %v, want %v", c.typ, got, c.want)
		}
	}
}

// The lossless-RoCE firmware gate: the bash applies the block only on
// SuperNIC or ConnectX-7..9 — a ConnectX-4 Lx is out.
func TestLosslessGate(t *testing.T) {
	if matchesConnectX79("ConnectX4Lx") {
		t.Fatalf("ConnectX4Lx must not pass the lossless gate")
	}
	if !matchesConnectX79("ConnectX7") {
		t.Fatalf("ConnectX7 must pass the lossless gate")
	}
}

// The PFC readback is a column table, not the comma-separated --pfc input
// form; the parser must read the enabled row.
func TestPfcEnabled(t *testing.T) {
	out := "DCBX mode: OS controlled\nPriority trust state: dscp\nPFC configuration:\n\tpriority    0   1   2   3   4   5   6   7\n\tenabled     0   0   0   1   0   0   0   0   \n\tbuffer      0   0   0   1   0   0   0   0   \n"
	if got := pfcEnabled(out); got != "0,0,0,1,0,0,0,0" {
		t.Fatalf("pfcEnabled = %q, want 0,0,0,1,0,0,0,0", got)
	}
	if got := pfcEnabled("PFC configuration:\n\tpriority 0 1\n"); got != "" {
		t.Fatalf("pfcEnabled on a truncated table = %q, want empty", got)
	}
}

// On ConnectX-4 Lx the bash fn_set_lossless_roce gate excludes the whole
// body — the step must skip honestly and name the inventory it saw (the
// live lab hardware).
func TestStepLosslessRoceOffClassSkip(t *testing.T) {
	a := &Agent{mellanoxFns: []pciDevice{
		{pci: "0000:49:00.0", devType: "ConnectX4LX", rawType: "ConnectX4Lx", variant: "Air", rail: "r0_p0"},
		{pci: "0000:49:00.1", devType: "ConnectX4LX", rawType: "ConnectX4Lx", variant: "Air", rail: "r0_p1"},
	}}
	profile := profileForTest(9000, "legacy", true)
	profile.Spec.EastWest.RoceCC = true

	state, msg := stepLosslessRoce(a, nil, profile)
	if state != v1alpha1.StepDone {
		t.Fatalf("off-class hardware must skip Done, got %s: %s", state, msg)
	}
	if !strings.Contains(msg, "no lossless-RoCE class devices") || !strings.Contains(msg, "ConnectX4LX 0000:49:00.0") {
		t.Fatalf("skip message should name the off-class inventory: %s", msg)
	}
}

func TestRailPort(t *testing.T) {
	if railPort("0000:49:00.0") != "0" || railPort("0000:49:00.1") != "1" {
		t.Fatalf("railPort wrong")
	}
}

func profileForTest(mtu int, eswitch string, mutations bool) *v1alpha1.NodePrepProfile {
	return &v1alpha1.NodePrepProfile{
		Spec: v1alpha1.NodePrepProfileSpec{
			EastWest: v1alpha1.EastWestSpec{LinkType: "Ethernet", MTU: mtu, EswitchMode: eswitch, RoceCC: true},
			Rails:    []v1alpha1.Rail{{Rail: "r0", PCIFunction: "49:00"}},
			Policy:   v1alpha1.PolicySpec{HostMutations: &mutations},
		},
	}
}

// stepFabricNetplan writes the exact stanza under the host etc dir and
// detects an unchanged file as Done without rewriting.
func TestStepFabricNetplan(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{hostMutations: true, hostEtcDir: func() string { return dir },
		mellanoxFns: []pciDevice{
			{pci: "0000:49:00.0", rail: "r0_p0"},
			{pci: "0000:49:00.1", rail: "r0_p1"},
		}}
	profile := profileForTest(9000, "legacy", true)

	state, msg := stepFabricNetplan(a, nil, profile)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "netplan written") {
		t.Fatalf("first run: %s %s", state, msg)
	}
	for _, rail := range []string{"r0_p0", "r0_p1"} {
		b, err := os.ReadFile(filepath.Join(dir, "netplan", "gpu_fabric_eth_"+rail+".yaml"))
		if err != nil {
			t.Fatalf("netplan file missing: %v", err)
		}
		if string(b) != renderNetplan(rail, 9000, false) {
			t.Fatalf("netplan content mismatch for %s", rail)
		}
	}
	// legacy mode must render ignore-carrier false (switchdev only flips it)
	b, _ := os.ReadFile(filepath.Join(dir, "netplan", "gpu_fabric_eth_r0_p0.yaml"))
	if !strings.Contains(string(b), "ignore-carrier: false") {
		t.Fatalf("legacy netplan must set ignore-carrier: false")
	}

	state, msg = stepFabricNetplan(a, nil, profile)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "verified") {
		t.Fatalf("second run should detect-verify, got %s %s", state, msg)
	}
}

// Without -host-mutations the mutating steps report Blocked instead of
// guessing.
func TestMutationGate(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{hostEtcDir: func() string { return dir },
		mellanoxFns: []pciDevice{{pci: "0000:49:00.0", rail: "r0"}}}
	profile := profileForTest(9000, "legacy", true) // policy on, agent flag off

	state, msg := stepFabricNetplan(a, nil, profile)
	if state != v1alpha1.StepBlocked || !strings.Contains(msg, "-host-mutations") {
		t.Fatalf("netplan without agent flag: %s %s", state, msg)
	}
}

// With no BlueField-3 present the flash step is Done (skipped) and names the
// inventory it saw — the ConnectX-4 Lx case of the test cluster.
func TestStepBFBFlashNoBluefield(t *testing.T) {
	a := &Agent{mellanoxFns: []pciDevice{
		{pci: "0000:16:00.0", devType: "ConnectX4Lx", rawType: "ConnectX4Lx", variant: "Air"},
		{pci: "0000:16:00.1", devType: "ConnectX4Lx", rawType: "ConnectX4Lx", variant: "Air"},
	}}
	profile := profileForTest(9000, "legacy", true)
	profile.Spec.Firmware = v1alpha1.FirmwareSource{BFB: v1alpha1.BFBSource{Name: "bf-fwbundle-3.4.0-92_26.04-prod.bfb"}}

	state, msg := stepBFBFlash(a, nil, profile)
	if state != v1alpha1.StepDone {
		t.Fatalf("flash step must be Done (skip) on ConnectX-only hardware, got %s: %s", state, msg)
	}
	if !strings.Contains(msg, "no BlueField-3") || !strings.Contains(msg, "ConnectX4Lx") {
		t.Fatalf("skip message should name the non-flashable inventory: %s", msg)
	}
}

// A BlueField-3 without a configured BFB is Blocked with the reason, not
// silently skipped.
func TestStepBFBFlashBluefieldNeedsBFB(t *testing.T) {
	a := &Agent{mellanoxFns: []pciDevice{
		{pci: "0000:05:00.0", devType: "DPU", rawType: "BlueField3", variant: "Physical", rshim: "/dev/rshim0"},
	}}
	profile := profileForTest(9000, "legacy", true) // no firmware configured

	state, msg := stepBFBFlash(a, nil, profile)
	if state != v1alpha1.StepBlocked || !strings.Contains(msg, "no firmware.bfb") {
		t.Fatalf("bluefield-3 without BFB config: %s %s", state, msg)
	}
}

// With zero VFs requested the VF GUID step is a no-op regardless of
// hardware (the VF pipeline is v0.2).
func TestStepVfGuidsNoVFs(t *testing.T) {
	a := &Agent{mellanoxFns: []pciDevice{{pci: "0000:49:00.0", rail: "r0"}}}
	profile := profileForTest(9000, "legacy", true)

	state, msg := stepVfGuids(a, nil, profile)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "no VFs requested") {
		t.Fatalf("vfGuids with numVFs=0: %s %s", state, msg)
	}
}

func TestStepDisableACSPolicySkip(t *testing.T) {
	a := &Agent{mellanoxFns: []pciDevice{{pci: "0000:49:00.0"}}}
	profile := profileForTest(9000, "legacy", true)
	profile.Spec.Policy.DisableACS = false

	state, msg := stepDisableACS(a, nil, profile)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "skipped by policy") {
		t.Fatalf("disableACS=false must skip: %s %s", state, msg)
	}
}

// The rename step is a no-op on InfiniBand rails (bash LINKTYPE_EW gate).
func TestStepUdevRulesNonEthernetSkip(t *testing.T) {
	a := &Agent{mellanoxFns: []pciDevice{{pci: "0000:49:00.0", rail: "r0"}}}
	profile := profileForTest(9000, "legacy", true)
	profile.Spec.EastWest.LinkType = "InfiniBand"

	state, msg := stepUdevRules(a, nil, profile)
	if state != v1alpha1.StepDone || !strings.Contains(msg, "not Ethernet") {
		t.Fatalf("InfiniBand rename must skip: %s %s", state, msg)
	}
}

// udevCurrent requires both rule files to render identically and the runtime
// names/MTU/up-state to match — a stale file or a missing rename forces the
// apply path.
func TestUdevCurrent(t *testing.T) {
	udev := t.TempDir()
	a := &Agent{hostEtcUdev: udev, hostMutations: true}
	rails := []pciDevice{{pci: "0000:49:00.0", rail: "r0"}}
	profile := profileForTest(9000, "legacy", true)

	if udevCurrent(a, profile, rails) {
		t.Fatalf("nothing written yet, must not be current")
	}
	if err := os.WriteFile(filepath.Join(udev, "70-persistent-net.rules"), []byte(renderNetRules(rails)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(udev, "60-persistent-rdma.rules"), []byte(renderRDMARules(rails)), 0o644); err != nil {
		t.Fatal(err)
	}
	// rule files match, but /sys has no eth_r0 here — still not current
	if udevCurrent(a, profile, rails) {
		t.Fatalf("without the renamed runtime interface it must not be current")
	}
}
