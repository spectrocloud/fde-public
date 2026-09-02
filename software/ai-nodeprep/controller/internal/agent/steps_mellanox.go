package agent

import (
	"fmt"
	"os"
	"path/filepath"
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
		if d.isDPU() && !profile.Spec.Policy.ControlDPU {
			a.logf("mlxconfig: control of DPUs is not allowed by policy, skipping %s", d.pci)
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
			fmt.Sprintf("mlxconfig applied to %s; reboot required for SR-IOV/eswitch config", strings.Join(configured, ",")))
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
	if _, err := a.hostExec(nil, 120*time.Second, "mlxconfig", "-d", d.pci, "-y", "reset"); err != nil {
		return true, append(errs, fmt.Sprintf("%s: reset: %v", d.pci, err))
	}
	args := []string{"-d", d.pci, "-y", "set"}
	for _, kv := range flash {
		args = append(args, kv.key+"="+kv.val)
	}
	if _, err := a.hostExec(nil, 120*time.Second, "mlxconfig", args...); err != nil {
		return true, append(errs, fmt.Sprintf("%s: set: %v", d.pci, err))
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

// udevCurrent reports whether the rename config and runtime state already
// match: rule files render identically, every rail function carries its
// eth_<rail> name, is up with the profile MTU, and has its roce_<rail> IB
// device.
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
	}
	return true
}

// stepUdevRules implements fn_rename_devices: render the rename rules, reload
// udev, trigger the renames, then bring every PF up with the profile MTU.
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
	return v1alpha1.StepDone, fmt.Sprintf("rail interfaces renamed and up: %s (mtu %d)", railNames(rails), mtu)
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
	if _, err := findHostTool("bfb-install"); err != nil {
		return v1alpha1.StepBlocked, fmt.Sprintf("bfb-install not found on host: %v", err)
	}
	if !a.mutationsAllowed(profile) {
		return v1alpha1.StepBlocked, "BFB flash requires -host-mutations (detect-only run)"
	}
	return v1alpha1.StepBlocked, fmt.Sprintf("BFB flash apply to %s lands in v0.2", strings.Join(bf3, ", "))
}

// stepVfGuids synthesises VF GUIDs when the profile requests VFs; with zero
// VFs the whole VF pipeline is a no-op.
func stepVfGuids(a *Agent, np *v1alpha1.NodePrep, profile *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
	if profile.Spec.EastWest.NumVFs+profile.Spec.NorthSouth.NumVFs == 0 {
		return v1alpha1.StepDone, "skipped: no VFs requested"
	}
	return stepNeedsMFT("mlxconfig/sysfs", "VF GUID synthesis (design §8.3)")(a, np, profile)
}
