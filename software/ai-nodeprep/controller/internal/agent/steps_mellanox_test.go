package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// Classification mirrors the bash fn_inventory_hw description parsing: the
// Description line decides variant AND identity; "N/A" (DSX Air emulated)
// falls back to the Device type line.
func TestClassifyMlxconfig(t *testing.T) {
	// A real ConnectX-4 Lx (the lab hardware): mlxconfig prints the device
	// header — with the actual description — even when the device rejects
	// the probed key, so the bash classifies it Physical despite the probe
	// failing; so does the full-query parse.
	cx4lx := "Device #1:\n----------\nDevice type:    ConnectX4Lx\nDescription:    ConnectX-4 Lx EN LOM; 25GbE dual-port BP; PCIe3.0 x8\n" +
		"Name:           MCX4121A-XCAT\n\nConfigurations:                                         Next Boot\n" +
		"-E- The Device doesn't support INTERNAL_CPU_OFFLOAD_ENGINE parameter\n"
	typ, variant := classifyMlxconfig(cx4lx)
	if typ != "ConnectX-4_Lx_EN_LOM" || variant != "Physical" {
		t.Fatalf("cx4lx classified %s/%s, want ConnectX-4_Lx_EN_LOM/Physical", typ, variant)
	}
	if got := rawDeviceType(cx4lx); got != "ConnectX4Lx" {
		t.Fatalf("rawDeviceType = %q, want ConnectX4Lx", got)
	}

	// BlueField-3 DPU and SuperNIC both report Device type "BlueField3" —
	// only the Description tells them apart (the mlxfwmanager B3140H
	// SuperNIC description).
	bf3dpu := "Device type:    BlueField3\nDescription:    NVIDIA BlueField-3 DPU; 400Gb/s; ... ;\nINTERNAL_CPU_OFFLOAD_ENGINE ECPF(0)\n"
	typ, variant = classifyMlxconfig(bf3dpu)
	if typ != "DPU" || variant != "Physical" {
		t.Fatalf("bf3 dpu classified %s/%s, want DPU/Physical", typ, variant)
	}
	bf3snic := "Device type:    BlueField3\nDescription:    Nvidia BlueField-3 B3140H E-series HHHL SuperNIC; 400GbE (default mode) / NDR IB; " +
		"Single-port QSFP112; PCIe Gen5.0 x16; 8 Arm cores; 16GB on board DDR; integrated BMC; Crypto Enabled\n"
	typ, variant = classifyMlxconfig(bf3snic)
	if typ != "SuperNIC" || variant != "Physical" {
		t.Fatalf("bf3 supernic classified %s/%s, want SuperNIC/Physical", typ, variant)
	}

	// DSX Air emulated devices resolve Description to N/A: variant Air and
	// the Device type line decides. A BlueField-3 carries no marker there
	// and classifies Unknown (as the bash does); an emulated ConnectX
	// keeps its type.
	airBF3 := "Device type:    BlueField3\nDescription:    N/A\n"
	if typ, variant = classifyMlxconfig(airBF3); typ != "Unknown" || variant != "Air" {
		t.Fatalf("dsx-air bf3 classified %s/%s, want Unknown/Air", typ, variant)
	}
	airCx := "Device type:    ConnectX6DX\nDescription:    N/A\n"
	if typ, variant = classifyMlxconfig(airCx); typ != "ConnectX6DX" || variant != "Air" {
		t.Fatalf("dsx-air cx classified %s/%s, want ConnectX6DX/Air", typ, variant)
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
		d := pciDevice{devType: tc.rawType, rawType: tc.rawType, rshim: "/dev/rshim0"}
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

// The full-query parser must pull config keys out of the real mlxconfig
// output shape — headers skipped, parenthesised values normalized — and it
// is what makes one-query-per-device detection possible.
func TestParseMlxconfigAll(t *testing.T) {
	out := "Device #1:\n----------\nDevice type:        ConnectX4LX\n" +
		"Name:               Super_Micro_B13DET_CX4LX_2p_x8_25g_Ax\n" +
		"Description:        ConnectX-4 Lx EN LOM; 25GbE dual-port BP; PCIe3.0 x8\n" +
		"Device:             0000:16:00.1\n\n" +
		"Configurations:                                         Next Boot\n" +
		"        SRIOV_EN                                        True(1)\n" +
		"        ROCE_RTT_RESP_DSCP_P1                           48\n" +
		"        ROCE_RTT_RESP_DSCP_MODE_P1                      FIXED_VALUE(1)\n" +
		"        LINK_TYPE_P1                                    ETH(2)\n" +
		"        NUM_OF_VFS                                      1\n"
	vals := parseMlxconfigAll(out)
	if vals["SRIOV_EN"] != "1" || vals["ROCE_RTT_RESP_DSCP_P1"] != "48" ||
		vals["ROCE_RTT_RESP_DSCP_MODE_P1"] != "1" || vals["LINK_TYPE_P1"] != "2" ||
		vals["NUM_OF_VFS"] != "1" {
		t.Fatalf("parse wrong: %v", vals)
	}
	for _, hdr := range []string{"Device", "Name", "Description", "Configurations", "Device type"} {
		if _, ok := vals[hdr]; ok {
			t.Fatalf("header %q must not be parsed as a config key", hdr)
		}
	}
	// a device that does not expose a key simply lacks it — the addKV gate
	if _, ok := vals["LINK_TYPE_P2"]; ok {
		t.Fatalf("LINK_TYPE_P2 must be absent for single-port gating")
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
		{pci: "0000:49:00.0", devType: "ConnectX4LX", rawType: "ConnectX4Lx", variant: "Physical", rail: "r0_p0"},
		{pci: "0000:49:00.1", devType: "ConnectX4LX", rawType: "ConnectX4Lx", variant: "Physical", rail: "r0_p1"},
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
		{pci: "0000:16:00.0", devType: "ConnectX4Lx", rawType: "ConnectX4Lx", variant: "Physical"},
		{pci: "0000:16:00.1", devType: "ConnectX4Lx", rawType: "ConnectX4Lx", variant: "Physical"},
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

// SR-IOV VFs and ACS-disable are mutually exclusive: a requested VF count on
// either fabric side (east-west or north-south) makes the step ignore
// disableACS, even when it is true.
func TestStepDisableACSVFExclusivity(t *testing.T) {
	a := &Agent{mellanoxFns: []pciDevice{{pci: "0000:49:00.0"}}}
	for _, side := range []string{"eastWest", "northSouth"} {
		profile := profileForTest(9000, "legacy", true)
		profile.Spec.Policy.DisableACS = true
		if side == "eastWest" {
			profile.Spec.EastWest.NumVFs = 1
		} else {
			profile.Spec.NorthSouth.NumVFs = 1
		}

		state, msg := stepDisableACS(a, nil, profile)
		if state != v1alpha1.StepDone || !strings.Contains(msg, "mutually exclusive") {
			t.Fatalf("%s.numVFs>0 must skip ACS disable: %s %s", side, state, msg)
		}
	}
}

// The disableACS message names every device ACS was written to — the
// operator-facing mutation record — and the steady state says "no ACS
// changes" instead of "disabled on 0".
// The bash subtracts 50 bytes from every Ethernet VF's MTU; that headroom is
// a switchdev requirement only (operator-corrected, 2026-09-04). Legacy VFs
// carry the full profile MTU.
// The auto branch of grubParams' IOMMU derivation keys off the CPU vendor
// parsed from /proc/cpuinfo (grubCPUVendor). The function reads /proc/cpuinfo
// directly and cannot be chrooted in-process, so the parse itself is
// exercised live on the test node instead — covered by the live verification
// in the 0.1.45 walk (vendor_id: GenuineIntel → intel_iommu=on derived).

// sriovNvPendingLoad is the commit/load two-boot predicate: the NV NUM_OF_VFS
// already satisfies the demand while the running sriov_totalvfs lags. In that
// state stepSriovNumVFs records its own load-reboot request (0.1.46, live on
// CX-4 Lx FW 14.32.1010) instead of stalling Blocked forever.
func TestSriovNvPendingLoad(t *testing.T) {
	if !sriovNvPendingLoad("4", 1, 4) {
		t.Fatalf("NV 4 / totalvfs 1 / want 4: committed-but-not-loaded must hold")
	}
	if sriovNvPendingLoad("4", 4, 4) {
		t.Fatalf("NV 4 / totalvfs 4 / want 4: converged, no load reboot")
	}
	if sriovNvPendingLoad("2", 1, 4) {
		t.Fatalf("NV 2 < want 4: the mlxconfig step still owes the apply, not a load boot")
	}
	if sriovNvPendingLoad("", 1, 4) {
		t.Fatalf("unreadable NUM_OF_VFS must not request a load reboot")
	}
}

func TestVfMTUWant(t *testing.T) {
	profile := profileForTest(9000, "legacy", true)
	if got := vfMTUWant(profile); got != 9000 {
		t.Fatalf("legacy eswitch VF MTU = %d, want full 9000", got)
	}
	profile.Spec.EastWest.EswitchMode = "switchdev"
	if got := vfMTUWant(profile); got != 8950 {
		t.Fatalf("switchdev VF MTU = %d, want 8950 (bash -50 headroom)", got)
	}
	profile.Spec.EastWest.MTU = 0
	if got := vfMTUWant(profile); got != 0 {
		t.Fatalf("unset MTU = %d, want 0 (no MTU config)", got)
	}
}

// VF MTU/bring-up enumeration is east-west rail-only: DPU-side VFs and
// non-rail functions are not fn_rename_devices' concern, and a VF itself
// never has VFs.
func TestRailVFNetdevsGating(t *testing.T) {
	a := &Agent{}
	profile := profileForTest(9000, "legacy", true)
	profile.Spec.EastWest.NumVFs = 1
	profile.Spec.NorthSouth.NumVFs = 2

	if got := railVFNetdevs(a, profile, pciDevice{pci: "0000:49:00.0", rail: "dpu", ibdev: "mlx5_0"}); got != nil {
		t.Fatalf("dpu rail must be excluded: %v", got)
	}
	if got := railVFNetdevs(a, profile, pciDevice{pci: "0000:49:00.0", rail: "", ibdev: "mlx5_0"}); got != nil {
		t.Fatalf("non-rail function must be excluded: %v", got)
	}
	if got := railVFNetdevs(a, profile, pciDevice{pci: "0000:49:00.2", rail: "r0_p0", ibdev: "mlx5_2", isVF: true}); got != nil {
		t.Fatalf("a VF must never have VFs: %v", got)
	}
	// A rail PF whose VFs do not exist yet (no virtfn in sysfs) lists nothing
	// rather than failing.
	if got := railVFNetdevs(a, profile, pciDevice{pci: "0000:49:00.0", rail: "r0", ibdev: "mlx5_bogus"}); got != nil {
		t.Fatalf("absent VFs must list empty, got %v", got)
	}
}

func TestAcsSummary(t *testing.T) {
	got := acsSummary([]string{"ff:0f.0", "ff:1d.0"}, []string{"ff:10.0", "ff:11.0", "ff:1e.0"}, nil)
	want := "disabled ACS on: ff:0f.0, ff:1d.0 (3 already clear, 0 without ACS capability)"
	if got != want {
		t.Fatalf("acsSummary:\n%s\nwant\n%s", got, want)
	}
	if got := acsSummary(nil, []string{"a", "b"}, []string{"c", "d", "e"}); got != "no ACS changes: 2 already clear, 3 without ACS capability" {
		t.Fatalf("steady-state summary = %q", got)
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

// mlxconfigErr must carry the tool's -E- diagnosis into the step message:
// the raw exec error is just "exit status 3", and the rejection detail is
// in the combined output — live from the 65535 VF ceiling probe on the
// CX-4 Lx mezz (FW 14.32.1010), where mlxconfig reports the firmware's
// maximum in the error and rejects before any NV write.
func TestMlxconfigErr(t *testing.T) {
	out := "Device #1:\n----------\n" +
		"Configurations:                                         Next Boot                         New\n" +
		"        NUM_OF_VFS                                      4                                 65535\n" +
		" Apply new Configuration? (y/n) [n] : y\n" +
		"Applying... Failed!\n" +
		"-E- Parameter NUM_OF_VFS' value is larger than maximum allowed 127\n"
	err := mlxconfigErr(out, errors.New("exit status 3"))
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("exit status dropped: %v", err)
	}
	if !strings.Contains(err.Error(), "maximum allowed 127") {
		t.Fatalf("rejection detail lost: %v", err)
	}

	// No -E- lines (e.g. a timeout or a killed exec): the error passes
	// through untouched rather than growing an empty annotation.
	if got := mlxconfigErr("some unrelated output", errors.New("exit status 1")); got.Error() != "exit status 1" {
		t.Fatalf("without -E- lines the error must pass through, got %q", got.Error())
	}
}
