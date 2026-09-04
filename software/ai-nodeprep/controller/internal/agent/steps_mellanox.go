package agent

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// This file carries the Mellanox conversion of nodeprep-v105.sh: adapter
// firmware configuration (fn_config_stage), rail interface renaming
// (fn_rename_devices), lossless RoCE runtime settings (fn_set_lossless_roce),
// ACS disable (fn_disable_acs) and BFB flash gating (fn_init_sw_stage). All
// steps are detect → apply → verify and idempotent, so the post-boot
// bootVerify re-run re-applies exactly the runtime-only settings (PFC, ACS)
// that the bash script re-applies through its at-boot precomplete pass.

// linkTypeNum maps the profile linkType to the mlxconfig LINK_TYPE value
// (bash LINKTYPE_EW/LINKTYPE_NS: 2 = Ethernet, 1 = InfiniBand; Ethernet is
// the bash default).
func linkTypeNum(s string) int {
	if strings.EqualFold(strings.TrimSpace(s), "InfiniBand") {
		return 1
	}
	return 2
}

// fwVFCount mirrors the bash config-stage clamp: the firmware NUM_OF_VFS
// never goes below 1 (CNX_NUM_OF_VFS/DPU_NUM_OF_VFS) even when the profile
// requests none at the OS level.
func fwVFCount(n int) int {
	if n > 1 {
		return n
	}
	return 1
}

// mlxconfigKV is one firmware config item.
type mlxconfigKV struct {
	key string
	val string
}

// valN parses val as an int (NUM_OF_VFS values are always numeric).
func (kv mlxconfigKV) valN() int {
	n, _ := strconv.Atoi(kv.val)
	return n
}

// mlxconfigGet queries one key; ok=false means the device does not expose it.
// Values arrive as "2(Ethernet)" / "True(1)" / "ETH(2)" — the parenthesised
// content wins when it is numeric, mirroring what the bash script compares.
func (a *Agent) mlxconfigGet(pci, key string) (string, bool, error) {
	vals, err := a.mlxconfigGetAll(pci)
	if err != nil {
		return "", false, err
	}
	v, ok := vals[key]
	if !ok {
		return "", false, fmt.Errorf("key %s not in mlxconfig output", key)
	}
	return v, true, nil
}

// mlxconfigGetAll runs one full `mlxconfig q` per device and parses every
// KEY VALUE line. The per-key form costs one mlxconfig invocation per key
// (the Ready-phase verify passes re-read all of them every pass); a single
// full query carries the same lines. Keys the device does not expose are
// simply absent from the map. The query dumps the device's whole
// configuration table, so it runs quiet — the step message (and, with
// -verbose, the exec log) carries the detail.
func (a *Agent) mlxconfigGetAll(pci string) (map[string]string, error) {
	out, err := a.hostExecQuiet(nil, 30*time.Second, "mlxconfig", "-d", pci, "q")
	if err != nil {
		return nil, err
	}
	return parseMlxconfigAll(out), nil
}

// parseMlxconfigAll extracts the KEY VALUE config lines from a full
// mlxconfig query: UPPER_SNAKE keys only — headers like "Device type:" and
// "Configurations:" carry punctuation and are skipped. Values arrive as
// "2(Ethernet)" / "True(1)" / "ETH(2)"; the parenthesised content wins when
// it is numeric, mirroring what the bash script compares.
func parseMlxconfigAll(out string) map[string]string {
	vals := map[string]string{}
	for _, ln := range strings.Split(out, "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		key := fields[0]
		if strings.ContainsAny(key, ":#") || strings.ToUpper(key) != key || vals[key] != "" {
			continue
		}
		v := fields[len(fields)-1]
		if i := strings.Index(v, "("); i >= 0 && strings.HasSuffix(v, ")") {
			inner := v[i+1 : len(v)-1]
			if _, err := strconv.Atoi(inner); err == nil {
				v = inner
			} else {
				v = v[:i]
			}
		}
		vals[key] = v
	}
	return vals
}

// setpciACS reads or writes the ACS Control Register (offset +0x6.w of the
// ACS extended capability), returning the value as four hex digits. Reads
// sweep every PCI function on the node and most devices have no ACS
// capability at all — an expected setpci error per device — so they run
// quiet (hostExecQuiet); the step message counts the negatives. Writes are
// rare, real mutations and stay fully logged.
// setpciACS reads (write=="") or writes the ACS Control Register of one PCI
// function. Both directions run quiet: the sweep touches every PCI function
// on the node and the disableACS step message — which names every device
// written — is the record of what changed; per-device setpci log lines are
// hundreds of lines of noise per verify pass. A failed write is not lost:
// it surfaces in the step's Failed message with the device address and
// error.
func (a *Agent) setpciACS(bdf, write string) (string, error) {
	args := []string{"-v", "-s", bdf, "ECAP_ACS+0x6.w"}
	if write != "" {
		args[3] += "=" + write
	}
	out, err := a.hostExecQuiet(nil, 15*time.Second, "setpci", args...)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty setpci output")
	}
	return fields[len(fields)-1], nil
}

// stepMlxconfig implements the bash fn_config_stage firmware block: per
// device it builds the mlxconfig set for its class (SuperNIC, ConnectX
// Physical/Air, DPU), detects the current values, and only resets+sets when
// something differs — the bash unconditionally resets, which made re-runs
// churn firmware state for no benefit. Keys the device does not expose are
// dropped from the set (the bash would fail the whole set on them). A device
// that was changed raises RebootRequired: SR-IOV/eswitch firmware config
// only takes effect on reboot, as in the bash config→reboot→precomplete
// sequence.
func stepMlxconfig(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !a.hasMellanox() {
		return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
	}
	if _, err := findHostTool("mlxconfig"); err != nil {
		return v1alpha1.StepBlocked, fmt.Sprintf("Mellanox hardware present but mlxconfig not found: %v", err)
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "adapter firmware config requires -host-mutations (detect-only run)"
	}

	ew, ns := profile.Spec.EastWest, profile.Spec.NorthSouth
	lt, ltNS := linkTypeNum(ew.LinkType), linkTypeNum(ns.LinkType)
	roceCC := "0"
	if ew.RoceCC {
		roceCC = "1"
	}
	dpuOffload := "0"
	switch strings.ToLower(strings.TrimSpace(ns.OffloadEngine)) {
	case "sf":
		dpuOffload = "1"
	case "smf":
		dpuOffload = "2"
	}

	var configured, matched []string
	var failures []string
	for _, d := range a.mellanoxFns {
		if d.isVF {
			// VFs have no device NV of their own (mlxconfig query exits 3)
			// — their SR-IOV/GUID config is driven through the PF.
			a.logf("mlxconfig: %s is a virtual function; config is inherited from the PF, skipping", d.pci)
			continue
		}
		if d.isDPU() && !profile.Spec.Policy.ControlDPU {
			a.logf("mlxconfig: control of DPUs is not allowed by policy, skipping %s", d.pci)
			continue
		}
		// East-west firmware config (link type, VF count) only belongs on the
		// rail-mapped functions (spec.rails). The bash applied NUMVF_EW to
		// every ConnectX-class function, which would also reconfigure e.g.
		// the host's bond0 uplink card — not nodeprep's to touch (Kevin's
		// correction, 2026-09-03).
		if !d.isDPU() && d.rail == "" {
			a.logf("mlxconfig: %s is not rail-mapped in spec.rails; east-west firmware config skipped", d.pci)
			continue
		}
		// one full query per device gates the set build AND the drift check
		vals, err := a.mlxconfigGetAll(d.pci)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: query: %v", d.pci, err))
			continue
		}
		flash := buildFlashSet(a, d, vals, mlxconfigParams{lt: lt, ltNS: ltNS, roceCC: roceCC, dpuOffload: dpuOffload,
			cnxVFs: fwVFCount(ew.NumVFs), dpuVFs: fwVFCount(ns.NumVFs)})
		if len(flash) == 0 {
			continue
		}
		changed, errs := applyMlxconfig(a, d, flash, vals)
		failures = append(failures, errs...)
		if len(errs) > 0 {
			continue
		}
		if changed {
			// A set that carried NUM_OF_VFS opens the sriov-nv-stage
			// machine for this PF (0.1.51): sriovNumVFs reads the stage to
			// tell the apply-commits boot from the loads-it boot and halts
			// cold when the warm load does not land. Reboots.Total is the
			// anchor — sriovNumVFs advances the machine when the counter
			// passes it.
			for _, kv := range flash {
				if kv.key == "NUM_OF_VFS" {
					a.setSriovStage(np, d.pci, sriovNvStage{NV: kv.valN(), Reboots: np.Status.Reboots.Total, State: sriovStateStaged})
					break
				}
			}
			configured = append(configured, d.pci)
		} else {
			matched = append(matched, d.pci)
		}
	}
	if len(failures) > 0 {
		return v1alpha1.StepFailed, strings.Join(failures, "; ")
	}
	if len(configured) > 0 {
		a.requestRebootBg(v1alpha1.RebootMlxConfigApplied,
			fmt.Sprintf("mlxconfig applied to %s; reboot required for SR-IOV/eswitch config", strings.Join(configured, ",")), "")
		return v1alpha1.StepBlocked, fmt.Sprintf("firmware config written to %d device(s) (%s), %d already matched; reboot requested",
			len(configured), strings.Join(configured, ", "), len(matched))
	}
	return v1alpha1.StepDone, fmt.Sprintf("adapter firmware config verified on %d device(s)", len(matched))
}

// mlxconfigParams carries the resolved profile values into the per-device
// set builder.
type mlxconfigParams struct {
	lt, ltNS           int
	roceCC, dpuOffload string
	cnxVFs, dpuVFs     int
}

// addKV appends a config item, or drops it (logged) when the device does not
// expose the key. The bash sets every key unconditionally, which fails the
// entire set on hardware lacking one key; checking the device's query map
// first keeps the supported superset applied.
func addKV(a *Agent, d pciDevice, flash []mlxconfigKV, vals map[string]string, key, val string) []mlxconfigKV {
	if _, ok := vals[key]; !ok {
		a.logf("mlxconfig: %s does not expose %s, skipping key", d.pci, key)
		return flash
	}
	return append(flash, mlxconfigKV{key: key, val: val})
}

// buildFlashConfig assembles the per-device mlxconfig set, bash
// fn_config_stage lines 506-589. vals is the device's full mlxconfig query
// (one exec per device), used for the key-support gating.
func buildFlashSet(a *Agent, d pciDevice, vals map[string]string, p mlxconfigParams) []mlxconfigKV {
	var flash []mlxconfigKV
	flash = addKV(a, d, flash, vals, "SRIOV_EN", "1")
	// PF_NUM_OF_VF_VALID is deliberately NOT set (bash-faithful). Setting it
	// True was tried live on a Supermicro LOM (FW 14.32.1010) under the
	// theory that factory False made the firmware ignore NUM_OF_VFS. The
	// truth: with the flag True the firmware drops the SR-IOV PCIe
	// capability entirely at power-on (no sriov_totalvfs in sysfs), while a
	// sibling card's factory NV (flag False, NUM_OF_VFS=8, SRIOV_EN True)
	// shows VFs are honored without the flag. Leave it False. (The further
	// theory that warm reboots never load SR-IOV NV at all was corrected in
	// 0.1.46: this class commits a mlxconfig change on the boot after the
	// set and loads it on the boot after that — warm loads on some
	// hardware, cold-only on others; the sriov-nv-stage machine of 0.1.51
	// arbitrates.)
	isSuper := d.devType == "SuperNIC"
	isCx79 := matchesConnectX79(d.devType)
	if p.lt == 2 {
		flash = addKV(a, d, flash, vals, "ROCE_RTT_RESP_DSCP_P1", "48")
		flash = addKV(a, d, flash, vals, "ROCE_RTT_RESP_DSCP_MODE_P1", "1")
		if isSuper || isCx79 {
			flash = addKV(a, d, flash, vals, "ROCE_ADAPTIVE_ROUTING_EN", p.roceCC)
			flash = addKV(a, d, flash, vals, "USER_PROGRAMMABLE_CC", p.roceCC)
			flash = addKV(a, d, flash, vals, "TX_SCHEDULER_LOCALITY_MODE", "2")
			flash = addKV(a, d, flash, vals, "ROCE_CC_STEERING_EXT", "2")
		}
	}
	switch {
	case isSuper && d.variant == "Physical":
		flash = append(flash, mlxconfigKV{"LINK_TYPE_P1", strconv.Itoa(p.lt)})
		flash = append(flash, mlxconfigKV{"NUM_OF_VFS", strconv.Itoa(p.cnxVFs)})
		if p.lt == 2 {
			flash = addKV(a, d, flash, vals, "MULTIPATH_DSCP", "0")
		}
		flash = addP2(a, d, flash, vals, p.lt)
	case d.isConnectX() && d.variant == "Physical":
		flash = addKV(a, d, flash, vals, "LINK_TYPE_P1", strconv.Itoa(p.lt))
		flash = append(flash, mlxconfigKV{"NUM_OF_VFS", strconv.Itoa(p.cnxVFs)})
		if p.lt == 2 && isCx79 {
			flash = addKV(a, d, flash, vals, "MULTIPATH_DSCP", "0")
		}
		flash = addP2(a, d, flash, vals, p.lt)
	case d.isConnectX() && d.variant == "Air":
		flash = append(flash, mlxconfigKV{"LINK_TYPE_P1", strconv.Itoa(p.lt)})
		flash = append(flash, mlxconfigKV{"NUM_OF_VFS", strconv.Itoa(p.cnxVFs)})
		if p.lt == 2 {
			flash = addKV(a, d, flash, vals, "ROCE_CC_RTT_TIMESTAMP_FORMAT", "0")
		}
	case d.isDPU():
		flash = append(flash, mlxconfigKV{"LINK_TYPE_P1", strconv.Itoa(p.ltNS)})
		flash = append(flash, mlxconfigKV{"NUM_OF_VFS", strconv.Itoa(p.dpuVFs)})
		if p.ltNS == 2 {
			flash = addKV(a, d, flash, vals, "MULTIPATH_DSCP", "0")
		}
		flash = addKV(a, d, flash, vals, "INTERNAL_CPU_OFFLOAD_ENGINE", p.dpuOffload)
		flash = addP2(a, d, flash, vals, p.ltNS)
	default:
		a.logf("mlxconfig: %s class %s/%s has no config set, skipping", d.pci, d.devType, d.variant)
		return nil
	}
	return flash
}

// addP2 adds the port-2 keys when the device exposes LINK_TYPE_P2 (bash: a
// bare query decides; single-port devices drop the block).
func addP2(a *Agent, d pciDevice, flash []mlxconfigKV, vals map[string]string, lt int) []mlxconfigKV {
	if _, ok := vals["LINK_TYPE_P2"]; !ok {
		return flash
	}
	flash = append(flash, mlxconfigKV{"LINK_TYPE_P2", strconv.Itoa(lt)})
	if lt == 2 {
		flash = addKV(a, d, flash, vals, "ROCE_RTT_RESP_DSCP_P2", "48")
		flash = addKV(a, d, flash, vals, "ROCE_RTT_RESP_DSCP_MODE_P2", "1")
	}
	return flash
}

// applyMlxconfig detects drift, applies reset+set when needed, and verifies;
// it reports whether the device was written and any errors. vals is the
// device's full query already fetched by the caller (detect reads it; the
// post-set verify re-fetches once).
func applyMlxconfig(a *Agent, d pciDevice, flash []mlxconfigKV, vals map[string]string) (bool, []string) {
	var errs []string
	need := false
	for _, kv := range flash {
		cur, ok := vals[kv.key]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: key %s absent from query", d.pci, kv.key))
			continue
		}
		if cur != kv.val {
			need = true
		}
	}
	if !need {
		return false, nil
	}
	if out, err := a.hostExec(nil, 120*time.Second, "mlxconfig", "-d", d.pci, "-y", "reset"); err != nil {
		return true, append(errs, fmt.Sprintf("%s: reset: %v", d.pci, mlxconfigErr(out, err)))
	}
	args := []string{"-d", d.pci, "-y", "set"}
	for _, kv := range flash {
		args = append(args, kv.key+"="+kv.val)
	}
	out, err := a.hostExec(nil, 120*time.Second, "mlxconfig", args...)
	if err != nil {
		return true, append(errs, fmt.Sprintf("%s: set: %v", d.pci, mlxconfigErr(out, err)))
	}
	fresh, err := a.mlxconfigGetAll(d.pci)
	if err != nil {
		return true, append(errs, fmt.Sprintf("%s: verify query: %v", d.pci, err))
	}
	for _, kv := range flash {
		if cur := fresh[kv.key]; cur != kv.val {
			errs = append(errs, fmt.Sprintf("%s: verify %s: want %s got %q", d.pci, kv.key, kv.val, cur))
		}
	}
	return true, errs
}

// mlxconfigErr enriches a failed mlxconfig exec with the tool's own
// diagnosis. The exec error alone is Go's "exit status 3"; mlxconfig puts
// its -E- lines in the combined output — e.g. a set above the firmware's
// VF ceiling fails with "Parameter NUM_OF_VFS' value is larger than
// maximum allowed 127" (live, ConnectX-4 Lx FW 14.32.1010). The over-range
// set is rejected by input validation before any write (NV shadow verified
// unchanged across the probe), so reading the ceiling off a deliberately
// over-range set is safe — but the agent never needs to run one: when a
// real set is rejected, this carries the reason into the step message
// instead of dropping it on the exec floor.
func mlxconfigErr(out string, err error) error {
	var eLines []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "-E- ") {
			eLines = append(eLines, s)
		}
	}
	if len(eLines) == 0 {
		return err
	}
	return fmt.Errorf("%w; %s", err, strings.Join(eLines, "; "))
}

// netplanPath is the bash gpu_fabric_eth_<rail>.yaml location.
func netplanPath(hostEtc, rail string) string {
	return filepath.Join(hostEtc, "netplan", "gpu_fabric_eth_"+rail+".yaml")
}

// renderNetplan mirrors the bash heredoc byte for byte (including the
// trailing newline from echo -e) so detection compares exactly.
func renderNetplan(rail string, mtu int, ignoreCarrier bool) string {
	return fmt.Sprintf("network:\n  version: 2\n  ethernets:\n    eth_%s:\n      ignore-carrier: %t\n      mtu: %d\n", rail, ignoreCarrier, mtu)
}

// stepFabricNetplan writes the per-rail netplan stanza (bash fn_config_stage
// tail): MTU for east-west rail interfaces, ignore-carrier only under
// switchdev. netplan apply happens at boot (the boot hook / networkd), not
// during the prep — as in the bash, where the file is written in the config
// stage and consumed after the reboot.
func stepFabricNetplan(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !a.hasMellanox() {
		return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
	}
	mtu := profile.Spec.EastWest.MTU
	if mtu <= 0 {
		return v1alpha1.StepDone, "skipped: no east-west MTU configured"
	}
	rails := railFns(profile, a)
	if len(rails) == 0 {
		return v1alpha1.StepDone, "skipped: no rail NICs to configure"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "netplan writes require -host-mutations (detect-only run)"
	}
	ignoreCarrier := strings.EqualFold(profile.Spec.EastWest.EswitchMode, "switchdev")
	dir := filepath.Join(a.hostEtcDir(), "netplan")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("cannot create %s: %v", dir, err)
	}
	var written, matched []string
	for _, d := range rails {
		path := netplanPath(a.hostEtcDir(), d.rail)
		want := renderNetplan(d.rail, mtu, ignoreCarrier)
		if cur, err := os.ReadFile(path); err == nil && string(cur) == want {
			matched = append(matched, filepath.Base(path))
			continue
		}
		if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("write %s: %v", path, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return v1alpha1.StepFailed, fmt.Sprintf("chmod %s: %v", path, err)
		}
		written = append(written, filepath.Base(path))
	}
	if len(written) > 0 {
		return v1alpha1.StepDone, fmt.Sprintf("netplan written: %s (mtu %d, ignore-carrier %t); applied at boot", strings.Join(written, ", "), mtu, ignoreCarrier)
	}
	return v1alpha1.StepDone, fmt.Sprintf("netplan verified: %s", strings.Join(matched, ", "))
}

// renderNetRules renders the two udev rule files from the rail functions
// (bash fn_rename_devices): PF renames to eth_<rail>, RDMA devices to
// roce_<rail> with the RoCE ToS. Deterministic order (by PCI address) makes
// the rendered content stable across re-runs.
func renderNetRules(rails []pciDevice) string {
	out := "# managed by nodeprep agent (eth_rN rail renaming)\n"
	for _, d := range sortedByPci(rails) {
		out += fmt.Sprintf("ACTION==\"add\", KERNELS==\"%s\", SUBSYSTEM==\"net\", NAME=\"eth_%s\"\n", d.pci, d.rail)
	}
	return out
}

func renderRDMARules(rails []pciDevice) string {
	out := "# managed by nodeprep agent (roce_rN rail renaming)\n"
	for _, d := range sortedByPci(rails) {
		out += fmt.Sprintf("ACTION==\"add\", KERNELS==\"%s\", SUBSYSTEM==\"infiniband\", PROGRAM=\"rdma_rename %%k NAME_FIXED roce_%s\"\n", d.pci, d.rail)
		out += fmt.Sprintf("ACTION==\"add\", KERNELS==\"%s\", SUBSYSTEM==\"infiniband\", RUN+=\"/bin/sh -c 'cma_roce_tos -d roce_%s -t 96 > /dev/null && echo 96 > /sys/class/infiniband/roce_%s/tc/1/traffic_class'\"\n", d.pci, d.rail, d.rail)
	}
	return out
}

func sortedByPci(rails []pciDevice) []pciDevice {
	out := append([]pciDevice{}, rails...)
	sort.Slice(out, func(i, j int) bool { return out[i].pci < out[j].pci })
	return out
}

// linkIsUp reports the IFF_UP flag from sysfs (operstate lags carrier).
func linkIsUp(dev string) bool {
	b, err := os.ReadFile("/sys/class/net/" + dev + "/flags")
	if err != nil {
		return false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 0, 64)
	return err == nil && v&0x1 != 0
}

// renamedTo returns the current netdev of the PCI function.
func renamedTo(pci string) string {
	ents, err := os.ReadDir("/sys/bus/pci/devices/" + pci + "/net")
	if err != nil || len(ents) == 0 {
		return ""
	}
	return ents[0].Name()
}

// vfMTUWant computes the MTU for the VF netdevs of a rail-mapped PF.
// Corrected bash logic (operator-directed, 2026-09-04): fn_rename_devices
// subtracts 50 bytes from every Ethernet VF's MTU, but that headroom is a
// switchdev requirement — in legacy eswitch mode the VF carries the full
// profile MTU. 0 = no MTU configured (profile mtu unset).
func vfMTUWant(profile *v1alpha1.NodePrepProfile) int {
	mtu := profile.Spec.EastWest.MTU
	if mtu <= 0 {
		return 0
	}
	if profile.Spec.EastWest.EswitchMode == "switchdev" {
		return mtu - 50
	}
	return mtu
}

// vfNetdev is one existing VF netdev of a rail-mapped PF.
type vfNetdev struct {
	vf   int
	slot string // PCI address of the VF
	name string // current netdev name (renamed or kernel)
}

// railVFNetdevs lists the existing east-west VF netdevs of a rail-mapped PF
// (bash fn_rename_devices' VF block is east-west only — DPU VFs are not
// touched there). VFs without a netdev yet are skipped: sriovNumVFs and
// vfGuids run just before this step in the stage, and a still-probing VF is
// picked up on a later pass through the same check.
func railVFNetdevs(a *Agent, profile *v1alpha1.NodePrepProfile, d pciDevice) []vfNetdev {
	n := vfCountFor(profile, d)
	if n == 0 || d.isVF || !strings.HasPrefix(d.rail, "r") {
		return nil
	}
	var out []vfNetdev
	for vf := 0; vf < n; vf++ {
		slot, err := vfSlotName(d.ibdev, vf)
		if err != nil {
			continue
		}
		name := renamedTo(slot)
		if name == "" {
			continue
		}
		out = append(out, vfNetdev{vf: vf, slot: slot, name: name})
	}
	return out
}

// udevCurrent reports whether the rename config and runtime state already
// match: rule files render identically, every rail function carries its
// eth_<rail> name, is up with the profile MTU, and has its roce_<rail> IB
// device — and every existing rail VF netdev carries the VF MTU (full in
// legacy eswitch, -50 in switchdev) and is up.
func udevCurrent(a *Agent, profile *v1alpha1.NodePrepProfile, rails []pciDevice) bool {
	wantNet, wantRDMA := renderNetRules(rails), renderRDMARules(rails)
	got, err := os.ReadFile(filepath.Join(a.hostEtcUdev, "70-persistent-net.rules"))
	if err != nil || string(got) != wantNet {
		return false
	}
	got, err = os.ReadFile(filepath.Join(a.hostEtcUdev, "60-persistent-rdma.rules"))
	if err != nil || string(got) != wantRDMA {
		return false
	}
	mtu := profile.Spec.EastWest.MTU
	mtuVF := vfMTUWant(profile)
	for _, d := range rails {
		if renamedTo(d.pci) != "eth_"+d.rail {
			return false
		}
		if mtu > 0 {
			if m, err := readSysfsInt("/sys/class/net/eth_" + d.rail + "/mtu"); err != nil || m != mtu {
				return false
			}
		}
		if !linkIsUp("eth_" + d.rail) {
			return false
		}
		if _, err := os.Stat("/sys/class/infiniband/roce_" + d.rail); err != nil {
			return false
		}
		for _, v := range railVFNetdevs(a, profile, d) {
			if mtuVF > 0 {
				if m, err := readSysfsInt("/sys/class/net/" + v.name + "/mtu"); err != nil || m != mtuVF {
					return false
				}
			}
			if !linkIsUp(v.name) {
				return false
			}
		}
	}
	return true
}

// stepUdevRules implements fn_rename_devices: render the rename rules, reload
// udev, trigger the renames, then bring every PF up with the profile MTU and
// every rail VF netdev up with the VF MTU (full in legacy eswitch, -50 in
// switchdev — corrected bash logic, operator-directed 2026-09-04).
// The runtime pieces (trigger, ip link) are re-applied whenever the state
// drifted — including by the post-boot bootVerify pass, matching the bash
// script's at-boot precomplete re-run. Renaming only runs on Ethernet rails
// (bash LINKTYPE_EW == 2 gate).
func stepUdevRules(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if linkTypeNum(profile.Spec.EastWest.LinkType) != 2 {
		return v1alpha1.StepDone, "skipped: east-west link type is not Ethernet (rename is Ethernet-only)"
	}
	if !a.hasMellanox() {
		return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
	}
	rails := railFns(profile, a)
	if len(rails) == 0 {
		return v1alpha1.StepDone, "skipped: no rail NICs to rename"
	}
	if udevCurrent(a, profile, rails) {
		return v1alpha1.StepDone, fmt.Sprintf("rail interfaces verified: %s", railNames(rails))
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "udev rename rules require -host-mutations (detect-only run)"
	}
	if _, err := findHostTool("udevadm"); err != nil {
		return v1alpha1.StepBlocked, fmt.Sprintf("udevadm not found on host: %v", err)
	}

	// down first, as the bash does, so the udev ADD trigger renames cleanly
	for _, d := range sortedByPci(rails) {
		if cur := renamedTo(d.pci); cur != "" && cur != "eth_"+d.rail {
			_, _ = a.hostExec(nil, 15*time.Second, "ip", "link", "set", "dev", cur, "down")
		}
	}
	if err := os.WriteFile(filepath.Join(a.hostEtcUdev, "70-persistent-net.rules"), []byte(renderNetRules(rails)), 0o644); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("write 70-persistent-net.rules: %v", err)
	}
	if err := os.WriteFile(filepath.Join(a.hostEtcUdev, "60-persistent-rdma.rules"), []byte(renderRDMARules(rails)), 0o644); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("write 60-persistent-rdma.rules: %v", err)
	}
	if _, err := a.hostExec(nil, 30*time.Second, "udevadm", "control", "--reload"); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("udevadm control --reload: %v", err)
	}
	if _, err := a.hostExec(nil, 30*time.Second, "udevadm", "trigger", "--action=add", "--subsystem-match=net"); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("udevadm trigger net: %v", err)
	}
	if _, err := a.hostExec(nil, 30*time.Second, "udevadm", "trigger", "--action=add", "--subsystem-match=infiniband"); err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("udevadm trigger infiniband: %v", err)
	}
	time.Sleep(5 * time.Second)
	mtu := profile.Spec.EastWest.MTU
	var errs []string
	for _, d := range sortedByPci(rails) {
		if cur := renamedTo(d.pci); cur != "eth_"+d.rail {
			errs = append(errs, fmt.Sprintf("%s did not rename to eth_%s (is %q)", d.pci, d.rail, cur))
			continue
		}
		args := []string{"link", "set", "dev", "eth_" + d.rail}
		if mtu > 0 {
			args = append(args, "mtu", strconv.Itoa(mtu))
		}
		args = append(args, "up")
		if _, err := a.hostExec(nil, 15*time.Second, "ip", args...); err != nil {
			errs = append(errs, fmt.Sprintf("eth_%s bring-up: %v", d.rail, err))
		}
	}
	if len(errs) > 0 {
		return v1alpha1.StepFailed, strings.Join(errs, "; ")
	}
	// VF bring-up (bash fn_rename_devices): after the renames, every rail VF
	// netdev gets its MTU and is brought up. Corrected bash logic (2026-09-04):
	// the 50-byte headroom is a switchdev requirement — legacy VFs carry the
	// full profile MTU. In switchdev the VF representor gets the same -50
	// treatment, best-effort (reps only exist under switchdev).
	mtuVF := vfMTUWant(profile)
	var vfNames []string
	for _, d := range sortedByPci(rails) {
		for _, v := range railVFNetdevs(a, profile, d) {
			args := []string{"link", "set", "dev", v.name}
			if mtuVF > 0 {
				args = append(args, "mtu", strconv.Itoa(mtuVF))
			}
			args = append(args, "up")
			if _, err := a.hostExec(nil, 15*time.Second, "ip", args...); err != nil {
				errs = append(errs, fmt.Sprintf("%s bring-up: %v", v.name, err))
				continue
			}
			vfNames = append(vfNames, v.name)
			if profile.Spec.EastWest.EswitchMode == "switchdev" {
				rep := fmt.Sprintf("nic_vf%d_rep_%s", v.vf, d.rail)
				if _, err := os.Stat("/sys/class/net/" + rep); err == nil {
					if _, err := a.hostExec(nil, 15*time.Second, "ip", "link", "set", "dev", rep,
						"mtu", strconv.Itoa(mtuVF), "up"); err != nil {
						a.logf("udevRules: representor %s bring-up failed: %v", rep, err)
					}
				}
			}
		}
	}
	if len(errs) > 0 {
		return v1alpha1.StepFailed, strings.Join(errs, "; ")
	}
	msg := fmt.Sprintf("rail interfaces renamed and up: %s (mtu %d)", railNames(rails), mtu)
	if len(vfNames) > 0 {
		vfMsg := fmt.Sprintf("; VFs up: %s", strings.Join(vfNames, ", "))
		if mtuVF > 0 {
			vfMsg += fmt.Sprintf(" (mtu %d)", mtuVF)
		}
		msg += vfMsg
	}
	return v1alpha1.StepDone, msg
}

func railNames(rails []pciDevice) string {
	var names []string
	for _, d := range sortedByPci(rails) {
		names = append(names, "eth_"+d.rail)
	}
	return strings.Join(names, ", ")
}

// stepLosslessRoce implements fn_set_lossless_roce on the rail devices:
// PFC priority 3 + DSCP trust, ECN enables, CNP DSCP — verified by readback.
// The whole body is gated on the bash fn_set_lossless_roce device class
// (SuperNIC or ConnectX-7..9): on ConnectX-4 Lx — the lab hardware — the
// bash applies no lossless settings at all, and the ECN sysfs interface
// doesn't even exist there, so anything else is overreach. Devices outside
// the class are reported in the skip message; the Spectrum-X specific
// pieces (mlxreg ROCE_ACCL/PIPG/0x5006, doca_spcx_cc) are best-effort,
// logged rather than fatal.
func stepLosslessRoce(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if linkTypeNum(profile.Spec.EastWest.LinkType) != 2 {
		return v1alpha1.StepDone, "skipped: east-west link type is not Ethernet"
	}
	if !a.hasMellanox() {
		return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
	}
	if !profile.Spec.EastWest.RoceCC {
		return v1alpha1.StepDone, "skipped by policy (roceCC=false)"
	}
	rails := railFns(profile, a)
	if len(rails) == 0 {
		return v1alpha1.StepDone, "skipped: no rail NICs to configure"
	}
	lossless, offClass := losslessClass(rails)
	if len(lossless) == 0 {
		return v1alpha1.StepDone, "skipped: no lossless-RoCE class devices (ConnectX-7..9/SuperNIC only); rail inventory: " + strings.Join(offClass, ", ")
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "lossless RoCE settings require -host-mutations (detect-only run)"
	}
	if _, err := findHostTool("mlnx_qos"); err != nil {
		return v1alpha1.StepBlocked, fmt.Sprintf("mlnx_qos not found (install the mft package): %v", err)
	}

	var notes, errs []string
	for _, d := range sortedByPci(lossless) {
		dev := renamedTo(d.pci)
		if dev == "" {
			dev = d.netdev
		}
		if dev == "" {
			errs = append(errs, fmt.Sprintf("%s: no netdev for lossless RoCE", d.pci))
			continue
		}
		if _, err := a.hostExec(nil, 30*time.Second, "mlnx_qos", "-i", dev, "--pfc=0,0,0,1,0,0,0,0", "--trust=dscp"); err != nil {
			errs = append(errs, fmt.Sprintf("%s: mlnx_qos: %v", dev, err))
			continue
		}
		for n := 0; n <= 7; n++ {
			for _, dir := range []string{"roce_rp", "roce_np"} {
				if err := os.WriteFile(fmt.Sprintf("/sys/class/net/%s/ecn/%s/enable/%d", dev, dir, n), []byte("1"), 0o644); err != nil {
					notes = append(notes, fmt.Sprintf("%s ecn %s/%d: %v", dev, dir, n, err))
				}
			}
		}
		if err := os.WriteFile(fmt.Sprintf("/sys/class/net/%s/ecn/roce_np/cnp_dscp", dev), []byte("48"), 0o644); err != nil {
			notes = append(notes, fmt.Sprintf("%s cnp_dscp: %v", dev, err))
		}
		a.applySpectrumXCC(d, dev)
	}
	if len(errs) > 0 {
		return v1alpha1.StepFailed, strings.Join(errs, "; ")
	}
	// runtime settings are lost on reboot; bootVerify re-runs this step and
	// re-applies them, so verify the freshly applied state now.
	if msg := a.verifyLossless(lossless); msg != "" {
		return v1alpha1.StepFailed, msg
	}
	msg := fmt.Sprintf("lossless RoCE applied on %s (pfc 0,0,0,1,0,0,0,0, trust dscp, ecn enabled, cnp_dscp 48)", railNames(lossless))
	if len(notes) > 0 {
		msg += "; notes: " + strings.Join(notes, "; ")
	}
	return v1alpha1.StepDone, msg
}

// applySpectrumXCC carries the Spectrum-X-only block (adaptive routing,
// congestion control daemon, inter-packet gap, 0x5006 registers) with bash
// best-effort semantics: failures are logged, never fatal.
func (a *Agent) applySpectrumXCC(d pciDevice, dev string) {
	if _, err := a.hostExec(nil, 30*time.Second, "mlxreg", "-d", d.pci, "--reg_name", "ROCE_ACCL",
		"--set", "roce_adp_retrans_en=0x1,roce_tx_window_en=0x1,roce_slow_restart_en=0x0,roce_slow_restart_idle_en=0x0,adaptive_routing_forced_en=0x1", "--yes"); err != nil {
		a.logf("losslessRoce: %s mlxreg ROCE_ACCL: %v (best-effort)", d.pci, err)
	}
	ccTool := "/host/opt/mellanox/doca/tools/doca_spcx_cc"
	if _, err := os.Stat(ccTool); err == nil {
		// backgrounded exactly like the bash: nohup + logger
		_, _ = a.hostExec(nil, 15*time.Second, "sh", "-c",
			"nohup /opt/mellanox/doca/tools/doca_spcx_cc -d roce_"+d.rail+" 2>&1 | logger -t doca_spcx_cc_roce_"+d.rail+" &")
	} else {
		a.logf("losslessRoce: %s doca_spcx_cc not present, skipping congestion-control daemon", d.pci)
	}
	if _, err := a.hostExec(nil, 30*time.Second, "mlxreg", "-d", d.pci, "-y", "--set", "ipg=0x00000019",
		"--reg_name", "PIPG", "--indexes", "local_port=1,lp_msb=0,ipg_cap_index=0"); err != nil {
		a.logf("losslessRoce: %s mlxreg PIPG: %v (best-effort)", d.pci, err)
	}
	if _, err := a.hostExec(nil, 30*time.Second, "mlxreg", "-d", d.pci, "-y", "--reg_id", "0x5006",
		"--set", "0x0.8:4=2,0x0.16:8=1,0x4.8:1=1,0x4.31:1=1", "--reg_len", "16"); err != nil {
		a.logf("losslessRoce: %s mlxreg 0x5006 (2): %v (best-effort)", d.pci, err)
	}
	if _, err := a.hostExec(nil, 30*time.Second, "mlxreg", "-d", d.pci, "-y", "--reg_id", "0x5006",
		"--set", "0x0.8:4=1,0x0.16:8=1,0x4.8:1=1,0x4.31:1=1", "--reg_len", "16"); err != nil {
		a.logf("losslessRoce: %s mlxreg 0x5006 (1): %v (best-effort)", d.pci, err)
	}
}

// losslessClass splits rail devices into the bash fn_set_lossless_roce gate
// (SuperNIC or ConnectX-7..9) and the off-class remainder, labeled for
// reporting.
func losslessClass(rails []pciDevice) (lossless []pciDevice, offClass []string) {
	for _, d := range rails {
		if d.devType == "SuperNIC" || matchesConnectX79(d.devType) {
			lossless = append(lossless, d)
		} else {
			offClass = append(offClass, fmt.Sprintf("%s %s (%s/%s)", d.devType, d.pci, d.rawType, d.variant))
		}
	}
	return lossless, offClass
}

// pfcEnabled parses the mlnx_qos PFC readback table, which prints the
// per-priority enables as columns —
// "\tenabled     0   0   0   1   0   0   0   0" — not the comma form of the
// --pfc input. Returns the comma-joined enables, or "" when the table is
// missing from the output.
func pfcEnabled(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) == 9 && f[0] == "enabled" {
			return strings.Join(f[1:], ",")
		}
	}
	return ""
}

// verifyLossless reads back the core runtime settings; empty means verified.
func (a *Agent) verifyLossless(rails []pciDevice) string {
	var errs []string
	for _, d := range sortedByPci(rails) {
		dev := renamedTo(d.pci)
		if dev == "" {
			dev = d.netdev
		}
		if dev == "" {
			continue
		}
		out, err := a.hostExecQuiet(nil, 30*time.Second, "mlnx_qos", "-i", dev) // -verbose shows the readback
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: mlnx_qos query: %v", dev, err))
			continue
		}
		if got := pfcEnabled(out); got != "0,0,0,1,0,0,0,0" {
			errs = append(errs, fmt.Sprintf("%s: PFC readback is %q, want 0,0,0,1,0,0,0,0", dev, got))
		}
		for _, probe := range []struct {
			path string
			want string
		}{
			{fmt.Sprintf("/sys/class/net/%s/ecn/roce_rp/enable/3", dev), "1"},
			{fmt.Sprintf("/sys/class/net/%s/ecn/roce_np/enable/3", dev), "1"},
			{fmt.Sprintf("/sys/class/net/%s/ecn/roce_np/cnp_dscp", dev), "48"},
		} {
			b, err := os.ReadFile(probe.path)
			if err != nil || strings.TrimSpace(string(b)) != probe.want {
				errs = append(errs, fmt.Sprintf("%s: %s readback failed", dev, probe.path))
			}
		}
	}
	return strings.Join(errs, "; ")
}

// stepDisableACS implements fn_disable_acs: clear the ACS Control Register
// on every PCI device that exposes an ACS capability, with a readback
// verify. Runtime-only (resets on reboot) — bootVerify re-runs the step,
// matching the bash at-boot re-run. The enumeration and all setpci traffic
// run quiet; the step message is the record: every device ACS was disabled
// on is named, the benign buckets stay counts.
func stepDisableACS(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !profile.Spec.Policy.DisableACS {
		return v1alpha1.StepDone, "skipped by policy (disableACS=false)"
	}
	// SR-IOV VFs and ACS-disable are mutually exclusive (operator-directed,
	// 2026-09-04): when the profile requests VFs on either fabric side the
	// VF request wins and disableACS is ignored, however it is set.
	if profile.Spec.EastWest.NumVFs > 0 || profile.Spec.NorthSouth.NumVFs > 0 {
		return v1alpha1.StepDone, "skipped: profile requests SR-IOV VFs (eastWest/northSouth.numVFs); ACS-disable is mutually exclusive with VFs"
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "ACS disable requires -host-mutations"
	}
	if _, err := findHostTool("setpci"); err != nil {
		return v1alpha1.StepBlocked, fmt.Sprintf("setpci not found on host: %v", err)
	}
	out, err := a.hostExecQuiet(nil, 60*time.Second, "lspci", "-d", "*:*:*")
	if err != nil {
		return v1alpha1.StepFailed, fmt.Sprintf("lspci: %v", err)
	}
	var disabled, already, noACS, failed []string
	for _, ln := range strings.Split(out, "\n") {
		fields := strings.Fields(ln)
		if len(fields) == 0 {
			continue
		}
		bdf := fields[0]
		val, err := a.setpciACS(bdf, "")
		if err != nil {
			noACS = append(noACS, bdf) // bash: "does not support ACS, skipping"
			continue
		}
		if val == "0000" {
			already = append(already, bdf)
			continue
		}
		if _, err := a.setpciACS(bdf, "0000"); err != nil {
			failed = append(failed, fmt.Sprintf("%s write: %v", bdf, err))
			continue
		}
		val, err = a.setpciACS(bdf, "")
		if err != nil || val != "0000" {
			failed = append(failed, fmt.Sprintf("%s readback after write is %q", bdf, val))
			continue
		}
		disabled = append(disabled, bdf)
	}
	if len(failed) > 0 {
		return v1alpha1.StepFailed, "ACS disable failures: " + strings.Join(failed, "; ")
	}
	return v1alpha1.StepDone, acsSummary(disabled, already, noACS)
}

// acsSummary renders the disableACS step message. The devices ACS was
// actually disabled on are listed by address — that list is the step's
// mutation record, in the operator-facing form ("disabled ACS on: ff:0f.0,
// ff:1d.0 …"); already-clear and no-capability devices are counts, they
// would bury the list.
func acsSummary(disabled, already, noACS []string) string {
	if len(disabled) == 0 {
		return fmt.Sprintf("no ACS changes: %d already clear, %d without ACS capability", len(already), len(noACS))
	}
	return fmt.Sprintf("disabled ACS on: %s (%d already clear, %d without ACS capability)",
		strings.Join(disabled, ", "), len(already), len(noACS))
}

// stepBFBFlash gates the BFB flash on hardware that may actually be flashed:
// only BlueField-3 devices with an rshim are candidates (design §8.2). Other
// Mellanox hardware — ConnectX, BlueField-2, SuperNIC — is reported by name
// so an operator sees why nothing was flashed. The apply itself lands in
// v0.2; detection and gating are complete here.
func stepBFBFlash(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if !a.hasMellanox() {
		return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
	}
	var bf3, others []string
	for _, d := range a.mellanoxFns {
		label := fmt.Sprintf("%s %s (%s/%s)", d.devType, d.pci, d.rawType, d.variant)
		if d.isBluefield3() && d.rshim != "" {
			bf3 = append(bf3, label+" rshim "+d.rshim)
		} else {
			others = append(others, label)
		}
	}
	if len(bf3) == 0 {
		return v1alpha1.StepDone, fmt.Sprintf("skipped: no BlueField-3 present to flash (only flashable class); Mellanox inventory: %s", strings.Join(others, ", "))
	}
	fw := profile.Spec.Firmware
	if fw.BFB.Name == "" {
		return v1alpha1.StepBlocked, fmt.Sprintf("BlueField-3 present (%s) but no firmware.bfb configured", strings.Join(bf3, ", "))
	}
	// Firmware-version gate (profile firmware.version, e.g. "32.49.1014" —
	// the version inside the configured BFB): a BlueField-3 already running
	// the target version needs no flash and no reboot; a mismatch is the
	// upgrade trigger. The comparison needs the flint-reported running
	// version, which the MFT enrichment fills — an empty fwVer means
	// classification has not landed yet (MFT still installing), not a match.
	if fw.Version != "" {
		var unknown, mismatch, match []string
		for _, d := range a.mellanoxFns {
			if !d.isBluefield3() {
				continue
			}
			switch {
			case d.fwVer == "":
				unknown = append(unknown, d.pci)
			case d.fwVer != fw.Version:
				mismatch = append(mismatch, fmt.Sprintf("%s running %s, target %s", d.pci, d.fwVer, fw.Version))
			default:
				match = append(match, d.pci)
			}
		}
		if len(unknown) > 0 {
			return v1alpha1.StepBlocked, fmt.Sprintf("firmware version not yet readable on %s (MFT classification pending); re-checks on a later refresh", strings.Join(unknown, ", "))
		}
		if len(mismatch) > 0 {
			// The upgrade path: BFB flash apply, then a reboot (or device
			// reset — the reboot is the route taken here) to load the new
			// image. The flash body itself lands in v0.2; until then the
			// step stays Blocked naming the delta. A config apply (the
			// Configuring mlxconfig step) always runs after the upgrade
			// because the new image resets NV to its defaults.
			return v1alpha1.StepBlocked, fmt.Sprintf("firmware upgrade required: %s (BFB flash apply lands in v0.2; a reboot loads the new image)", strings.Join(mismatch, "; "))
		}
		if len(match) > 0 {
			return v1alpha1.StepDone, fmt.Sprintf("firmware version %s matches profile target on %s; no flash, no reboot", fw.Version, strings.Join(match, ", "))
		}
	}
	if _, err := findHostTool("bfb-install"); err != nil {
		return v1alpha1.StepBlocked, fmt.Sprintf("bfb-install not found on host: %v", err)
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "BFB flash requires -host-mutations (detect-only run)"
	}
	return v1alpha1.StepBlocked, fmt.Sprintf("BFB flash apply to %s lands in v0.2", strings.Join(bf3, ", "))
}

// vfIdentity synthesizes a VF's node GUID, port GUID and MAC from the PF's
// colon-stripped node GUID — byte-for-byte the bash fn_set_vfs L675-686
// formula (node: guid[0:7] + "f1" + vf, port: "f2", MAC: "f2" + vf +
// guid[8:]), each re-colonised by the caller's writer or the sysfs reader.
func vfIdentity(guid string, vf int) (node, port, mac string, err error) {
	if len(guid) != 16 {
		return "", "", "", fmt.Errorf("node GUID %q is not 16 hex digits", guid)
	}
	if _, err := hex.DecodeString(guid); err != nil {
		return "", "", "", fmt.Errorf("node GUID %q is not hex: %v", guid, err)
	}
	vh := fmt.Sprintf("%02x", vf)
	node = guid[:7] + "f1" + vh + guid[11:]
	port = guid[:7] + "f2" + vh + guid[11:]
	mac = "f2" + vh + guid[8:]
	return node, port, mac, nil
}

// vfClassGUID reports whether a device class gets VF GUID configuration
// (bash gates the GUID block on SuperNIC/ConnectX; DPUs take numvfs only).
func vfClassGUID(d pciDevice) bool {
	return d.devType == "SuperNIC" || d.isConnectX()
}

// stepVfGuids converges the per-VF identity onto the synthesized values
// (bash fn_set_vfs L655-774): node GUID on every requested VF, port GUID +
// policy Follow for IB links, MAC for Ethernet on legacy eswitch, PCI
// unbind/rebind to activate a change, and — for rail-mapped Ethernet VFs —
// the 71/61 udev rename rules. Detect-first: an all-match host reports Done
// without touching anything. Writes go through an unbind → write → bind
// window (applyVfGuids for why).
func stepVfGuids(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if profile.Spec.EastWest.NumVFs+profile.Spec.NorthSouth.NumVFs == 0 {
		return v1alpha1.StepDone, "skipped: no VFs requested"
	}
	if !a.hasMellanox() {
		return v1alpha1.StepDone, "skipped: no Mellanox hardware present"
	}
	plan, mismatches, blocked, failed := vfGuidPlan(a, profile)
	if len(failed) > 0 {
		return v1alpha1.StepFailed, fmt.Sprintf("cannot configure VF GUIDs: %s", strings.Join(failed, "; "))
	}
	if len(blocked) > 0 {
		// Transient: VFs not created yet (sriovNumVFs runs first in the
		// stage, but a Blocked sibling must not burn this step's attempts).
		return v1alpha1.StepBlocked, strings.Join(blocked, "; ")
	}
	if len(mismatches) == 0 {
		return v1alpha1.StepDone, fmt.Sprintf("VF GUIDs verified on %d VF(s) (%s)", plan.vfTotal, plan.summary)
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, fmt.Sprintf("VF identity needs writes on %d VF(s) (%s); requires -host-mutations and policy.hostMutations",
			len(mismatches), strings.Join(mismatches, "; "))
	}
	msg, err := a.applyVfGuids(profile, plan)
	if err != nil {
		return v1alpha1.StepFailed, err.Error()
	}
	return v1alpha1.StepDone, msg
}

// vfGuidPlan computes the desired per-VF identity for every GUID-class
// function and diffs it against the host. mismatches name the deviating
// attributes ("49:00.0 vf0 node"); blocked names transient states — a PF
// still re-probing (no IB device yet, unreadable node GUID) and VFs whose
// sriov sysfs dir does not exist yet (sriovNumVFs has not landed them) —
// that must not burn the step's attempts. Drift is repaired per attribute —
// an all-match VF is left untouched, never bounced.
func vfGuidPlan(a *Agent, profile *v1alpha1.NodePrepProfile) (plan vfGuidWork, mismatches, blocked, failed []string) {
	plan.profile = profile
	for _, nic := range a.mellanoxFns {
		n := vfCountFor(profile, nic)
		if n == 0 || !vfClassGUID(nic) {
			continue
		}
		ibdev := nic.ibdev
		if ibdev == "" {
			ibdev = ibdevFor(nic.pci)
		}
		if ibdev == "" {
			// Transient in practice: the PF's infiniband device vanishes
			// while the driver re-probes (function reset, VF churn — found
			// live when the apply reboot raced this step). Blocked, not
			// Failed, so a re-probing PF does not burn the 5 attempts.
			blocked = append(blocked, fmt.Sprintf("%s (%s): no IB device yet", nic.pci, nic.netdev))
			continue
		}
		lt := linkTypeNum(profile.Spec.EastWest.LinkType)
		// North-South (DPU) side has no eswitch knob in the API — bash
		// treats the DPU host side as legacy.
		esw := profile.Spec.EastWest.EswitchMode
		if nic.rail == "dpu" {
			lt = linkTypeNum(profile.Spec.NorthSouth.LinkType)
			esw = "legacy"
		}
		raw := ""
		if b, err := os.ReadFile("/sys/class/infiniband/" + ibdev + "/node_guid"); err == nil {
			raw = strings.ToLower(strings.NewReplacer(":", "", "\n", "", " ", "").Replace(string(b)))
		}
		if len(raw) != 16 {
			blocked = append(blocked, fmt.Sprintf("%s: unreadable node GUID on %s", nic.pci, ibdev))
			continue
		}
		dev := vfDevWork{nic: nic, ibdev: ibdev, ib: lt == 1, switchdev: esw == "switchdev", nodeGuid: raw}
		for vf := 0; vf < n; vf++ {
			node, port, mac, err := vfIdentity(raw, vf)
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s vf%d: %v", nic.pci, vf, err))
				continue
			}
			sriovDir := fmt.Sprintf("/sys/class/infiniband/%s/device/sriov/%d", ibdev, vf)
			if _, err := os.Stat(sriovDir); err != nil {
				blocked = append(blocked, fmt.Sprintf("%s vf%d: %s absent (sriovNumVFs creates it)", nic.pci, vf, sriovDir))
				continue
			}
			w := vfWork{vf: vf, node: node, port: port, mac: mac, sriovDir: sriovDir}
			for _, attr := range []struct {
				name, want, path string
			}{
				{"node", node, sriovDir + "/node"},
			} {
				if got := normGUIDFile(attr.path); got != attr.want {
					w.need = append(w.need, vfAttr{attr.name, attr.want, attr.path})
					mismatches = append(mismatches, fmt.Sprintf("%s vf%d %s", nic.fn, vf, attr.name))
				}
			}
			if dev.ib {
				if got := normGUIDFile(sriovDir + "/port"); got != port {
					w.need = append(w.need, vfAttr{"port", port, sriovDir + "/port"})
					mismatches = append(mismatches, fmt.Sprintf("%s vf%d port", nic.fn, vf))
				}
				if b, err := os.ReadFile(sriovDir + "/policy"); err != nil || strings.TrimSpace(string(b)) != "Follow" {
					w.need = append(w.need, vfAttr{"policy", "Follow", sriovDir + "/policy"})
					mismatches = append(mismatches, fmt.Sprintf("%s vf%d policy", nic.fn, vf))
				}
			} else if !dev.switchdev {
				// The sriov mac attr is write-only (reads return a usage
				// string), so drift is detected on the VF netdev's address.
				slot, err := vfSlotName(ibdev, vf)
				if err != nil {
					blocked = append(blocked, fmt.Sprintf("%s vf%d: %v", nic.pci, vf, err))
					continue
				}
				if got := vfNetdevMac(slot); got != mac {
					w.need = append(w.need, vfAttr{"mac", mac, sriovDir + "/mac"})
					mismatches = append(mismatches, fmt.Sprintf("%s vf%d mac", nic.fn, vf))
				}
			}
			dev.vfs = append(dev.vfs, w)
		}
		plan.devs = append(plan.devs, dev)
		plan.vfTotal += n
	}
	return plan, mismatches, blocked, failed
}

// normGUIDFile reads a sysfs GUID/MAC attribute and colon-strips it for
// comparison; unreadable counts as a mismatch so the apply path repairs it.
func normGUIDFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.NewReplacer(":", "", "\n", "", " ", "").Replace(string(b)))
}

// vfSlotName extracts a VF's PCI slot from the PF's virtfn uevent.
func vfSlotName(ibdev string, vf int) (string, error) {
	vfPci, err := readSysfsString(fmt.Sprintf("/sys/class/infiniband/%s/device/virtfn%d/uevent", ibdev, vf))
	if err != nil {
		return "", err
	}
	for _, kv := range strings.Fields(vfPci) {
		if strings.HasPrefix(kv, "PCI_SLOT_NAME=") {
			return strings.TrimPrefix(kv, "PCI_SLOT_NAME="), nil
		}
	}
	return "", fmt.Errorf("no PCI_SLOT_NAME in virtfn uevent")
}

// vfNetdevMac reads a VF's MAC from its netdev (the sriov mac sysfs attr is
// write-only, so reads go through /sys/.../net/*/address); "" when the VF
// has no netdev yet (driver still probing).
func vfNetdevMac(slot string) string {
	matches, err := filepath.Glob("/sys/bus/pci/devices/" + slot + "/net/*/address")
	if err != nil || len(matches) == 0 {
		return ""
	}
	return normGUIDFile(matches[0])
}

// vfAttr is one sysfs attribute of one VF to write.
type vfAttr struct {
	attr, val, path string
}

// vfWork is the plan for one VF.
type vfWork struct {
	vf              int
	node, port, mac string
	sriovDir        string
	need            []vfAttr
}

// vfDevWork is the plan for one PF.
type vfDevWork struct {
	nic       pciDevice
	ibdev     string
	ib        bool
	switchdev bool
	nodeGuid  string
	vfs       []vfWork
}

// vfGuidWork is the plan for the node.
type vfGuidWork struct {
	profile *v1alpha1.NodePrepProfile
	devs    []vfDevWork
	vfTotal int
	summary string
}

// applyVfGuids executes a vfGuidPlan: bounce only the VFs whose identity
// drifted (bash bounces every VF; a detect-first rerun must not bounce
// converged ones), verify the readback, and render the VF udev rules for
// rail-mapped Ethernet ports.
//
// A bounced VF is written through an unbind → write → bind window, not
// write-then-bounce: at bind the driver re-derives the node GUID (EUI-64
// from the VF MAC) whenever the vf entry carries no staged value, so a
// write while bound is wiped by the rebind, and a mac-only write in the
// unbound window resets node to the default (both live-verified on CX-4 Lx
// FW 14.32.1010). Writing mac also resets a previously staged node — so
// within the window node is written LAST (mac/port/policy first); every
// bounced VF gets its FULL identity set rewritten (node always, port/policy
// for IB, mac for ETH legacy) so bind stages all of them.
func (a *Agent) applyVfGuids(profile *v1alpha1.NodePrepProfile, plan vfGuidWork) (string, error) {
	var summaries []string
	netRules, rdmaRules := "", ""
	for _, dev := range plan.devs {
		touched := 0
		for _, w := range dev.vfs {
			if len(w.need) == 0 {
				continue
			}
			// Node last: the mac write resets a staged node GUID.
			var writes []vfAttr
			if dev.ib {
				writes = append(writes,
					vfAttr{"port", w.port, w.sriovDir + "/port"},
					vfAttr{"policy", "Follow", w.sriovDir + "/policy"})
			} else if !dev.switchdev {
				writes = append(writes, vfAttr{"mac", w.mac, w.sriovDir + "/mac"})
			}
			writes = append(writes, vfAttr{"node", w.node, w.sriovDir + "/node"})
			slot, err := vfSlotName(dev.ibdev, w.vf)
			if err != nil {
				return "", fmt.Errorf("%s vf%d: %v", dev.nic.pci, w.vf, err)
			}
			if err := a.writeSysfs("/sys/bus/pci/drivers/mlx5_core/unbind", slot+"\n"); err != nil {
				return "", fmt.Errorf("%s vf%d: unbind %s: %v", dev.nic.pci, w.vf, slot, err)
			}
			for _, attr := range writes {
				// node/port/mac go to the kernel colon-formatted (bash's
				// sed 's/../&:/g' — the mlx5 sysfs stores parse GUID/MAC
				// with %x:%x:...); policy is a plain word.
				val := attr.val
				if attr.attr != "policy" {
					val = colonFormat(val)
				}
				if err := a.writeSysfs(attr.path, val+"\n"); err != nil {
					// Leave the driver bound again before bailing out.
					_ = a.writeSysfs("/sys/bus/pci/drivers/mlx5_core/bind", slot+"\n")
					return "", fmt.Errorf("%s vf%d %s: %v", dev.nic.pci, w.vf, attr.attr, err)
				}
				if attr.attr == "mac" {
					// The FW regenerates the node GUID from a newly written
					// MAC asynchronously — a node write issued immediately
					// after is clobbered (live-verified on CX-4 Lx FW
					// 14.32.1010: mac → node without a gap leaves node at
					// EUI-64(mac); a ≥2s gap lets it land).
					time.Sleep(2 * time.Second)
				}
			}
			if err := a.writeSysfs("/sys/bus/pci/drivers/mlx5_core/bind", slot+"\n"); err != nil {
				return "", fmt.Errorf("%s vf%d: bind %s: %v", dev.nic.pci, w.vf, slot, err)
			}
			// Verify after the rebind settled: node/port through the sysfs
			// attrs, mac through the VF netdev (the sysfs mac is write-only).
			deadline := time.Now().Add(15 * time.Second)
			for {
				ok := normGUIDFile(w.sriovDir+"/node") == w.node
				if ok && dev.ib {
					ok = normGUIDFile(w.sriovDir+"/port") == w.port
				}
				if ok && !dev.ib && !dev.switchdev {
					ok = vfNetdevMac(slot) == w.mac
				}
				if ok || time.Now().After(deadline) {
					if !ok {
						return "", fmt.Errorf("%s vf%d: identity readback mismatch after rebind (node %s, want %s)",
							dev.nic.pci, w.vf, normGUIDFile(w.sriovDir+"/node"), w.node)
					}
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			touched++
		}
		// Rail-mapped Ethernet VFs get rename rules (bash writes these for
		// ETH + ^r[0-9]+ rails regardless of eswitch mode). In switchdev the
		// bash adds a representor rename rule from the devlink port table;
		// attempted best-effort, failure just skips the rule.
		if !dev.ib && regexp.MustCompile(`^r[0-9]+`).MatchString(dev.nic.rail) {
			for _, w := range dev.vfs {
				slot, err := vfSlotName(dev.ibdev, w.vf)
				if err != nil {
					return "", fmt.Errorf("%s vf%d: %v", dev.nic.pci, w.vf, err)
				}
				name := fmt.Sprintf("nic_vf%d_%s", w.vf, dev.nic.rail)
				rdmaName := fmt.Sprintf("roce_vf%d_%s", w.vf, dev.nic.rail)
				netRules += fmt.Sprintf("ACTION==\"add\", KERNELS==\"%s\", SUBSYSTEM==\"net\", NAME=\"%s\"\n", slot, name)
				rdmaRules += fmt.Sprintf("ACTION==\"add\", KERNELS==\"%s\", SUBSYSTEM==\"infiniband\", PROGRAM=\"rdma_rename %%k NAME_FIXED %s\"\n", slot, rdmaName)
				rdmaRules += fmt.Sprintf("ACTION==\"add\", KERNELS==\"%s\", SUBSYSTEM==\"infiniband\", RUN+=\"/bin/sh -c 'cma_roce_tos -d %s -t 96 > /dev/null && echo 96 > /sys/class/infiniband/%s/tc/1/traffic_class'\"\n", slot, rdmaName, rdmaName)
				if dev.switchdev {
					repName, repSwitchID, err := a.vfRepresentor(dev.nic.pci)
					if err != nil {
						a.logf("vfGuids: %s vf%d: no representor for rename rule: %v", dev.nic.pci, w.vf, err)
					} else {
						netRules += fmt.Sprintf("ACTION==\"add\", ATTR{phys_switch_id}==\"%s\", ATTR{phys_port_name}==\"%s\" SUBSYSTEM==\"net\", NAME=\"nic_vf%d_rep_%s\"\n",
							repSwitchID, repName, w.vf, dev.nic.rail)
					}
				}
			}
		}
		if touched > 0 {
			summaries = append(summaries, fmt.Sprintf("%s %d VF(s)", dev.nic.pci, touched))
		}
	}
	// Stale VF rules from a previous profile go first (bash L618-619), so a
	// config that no longer maps a rail does not leave rename rules behind.
	for _, f := range []string{"71-persistent-net-vf.rules", "61-persistent-rdma-vf.rules"} {
		if err := os.Remove(filepath.Join(a.hostEtcUdev, f)); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove %s: %v", f, err)
		}
	}
	if netRules != "" || rdmaRules != "" {
		if err := os.WriteFile(filepath.Join(a.hostEtcUdev, "71-persistent-net-vf.rules"), []byte(netRules), 0o644); err != nil {
			return "", fmt.Errorf("write 71-persistent-net-vf.rules: %v", err)
		}
		if err := os.WriteFile(filepath.Join(a.hostEtcUdev, "61-persistent-rdma-vf.rules"), []byte(rdmaRules), 0o644); err != nil {
			return "", fmt.Errorf("write 61-persistent-rdma-vf.rules: %v", err)
		}
	}
	plan.summary = strings.Join(summaries, ", ")
	return fmt.Sprintf("VF GUIDs/MACs applied: %s", strings.Join(summaries, ", ")), nil
}

// colonFormat inserts a colon after every two hex chars (aa:bb:...) — the
// format bash writes and the kernel's %x:%x:... sysfs parsers expect.
func colonFormat(hexStr string) string {
	var b strings.Builder
	for i := 0; i < len(hexStr); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexStr[i : i+2])
	}
	return b.String()
}

// vfRepresentor looks up the switchdev representor netdev of a VF from the
// host's devlink port table (bash: devlink port show | grep pci/<pf> |
// grep pcivf) and reads its phys_port_name / phys_switch_id.
func (a *Agent) vfRepresentor(pfPCI string) (physPortName, switchID string, err error) {
	out, err := a.hostExec(nil, 30*time.Second, "devlink", "port", "show")
	if err != nil {
		return "", "", err
	}
	rep := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "pci/"+pfPCI) && strings.Contains(line, "pcivf") {
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				rep = strings.TrimSuffix(fields[4], ":")
				break
			}
		}
	}
	if rep == "" {
		return "", "", fmt.Errorf("no pcivf representor for %s in devlink port show", pfPCI)
	}
	name, err := readSysfsString("/sys/class/net/" + rep + "/phys_port_name")
	if err != nil {
		return "", "", err
	}
	id, err := readSysfsString("/sys/class/net/" + rep + "/phys_switch_id")
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(name), strings.TrimSpace(id), nil
}
