package agent

import (
	"fmt"
	"os"
	"strings"
	"time"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// pciDevice is one enumerated PCI function of interest.
type pciDevice struct {
	pci       string // 0000:05:00.0
	fn        string // 05:00 (bash rail-key form)
	vendor    string // 0x15b3, 0x10de
	deviceID  string
	class     string
	rail      string // assigned rail (r0, r0_p0, dpu, ...) or ""
	linkWidth string
	linkSpeed string
	netdev    string
	// MFT-enriched fields (bash arrBF 4/5/7/10/12): devType is the bash
	// arrBF[i,4] classification (SuperNIC | DPU | ConnectX-N | Unknown),
	// rawType the literal mlxconfig "Device type:" value (ConnectX4Lx,
	// BlueField3, ...) used for model-specific gates, variant the bash
	// arrBF[i,10] (Physical | Air).
	devType string
	rawType string
	variant string
	rshim   string // /dev/rshimN when the function owns one (DPUs)
	ibdev   string // mlx5_0 from sysfs infiniband/
	fwVer   string
	psid    string
}

// railPort returns the bash port suffix ${pci/*\./}: the function number of
// the PCI address ("0000:49:00.0" -> "0").
func railPort(pci string) string {
	parts := strings.Split(pci, ".")
	return parts[len(parts)-1]
}

// isConnectX reports whether the device classifies as a ConnectX (non-DPU,
// non-SuperNIC) adapter, the bash `[[ type =~ ConnectX ]]` branch.
func (d pciDevice) isConnectX() bool {
	return strings.Contains(d.devType, "ConnectX")
}

// isDPU reports the bash `[ type == "DPU" ]` branch.
func (d pciDevice) isDPU() bool {
	return d.devType == "DPU"
}

// isBluefield3 reports a BlueField-3 device from the raw mlxconfig device
// type (ConnectX-2..4 and BlueField-2 exist, so a bare "bluefield" match is
// not enough — only BF-3 is flashable, design §8.2).
func (d pciDevice) isBluefield3() bool {
	t := strings.ToLower(d.rawType)
	return strings.Contains(t, "bluefield") && strings.Contains(t, "3")
}

// matchesConnectX79 implements the bash lossless-RoCE firmware gate
// `[[ type =~ ConnectX[-]?[7-9] ]]`.
func matchesConnectX79(devType string) bool {
	for _, gen := range []string{"7", "8", "9"} {
		if strings.Contains(devType, "ConnectX"+gen) || strings.Contains(devType, "ConnectX-"+gen) {
			return true
		}
	}
	return false
}

// scanInventory walks /sys/bus/pci/devices and classifies NVIDIA GPUs and
// Mellanox NICs from sysfs alone (design §8.4: no lspci fork). Mellanox
// classification beyond "Mellanox" needs MFT and is filled in by
// enrichMellanox. Rail naming follows the bash grammar: a card with one
// function maps to r<N>, a multi-function card to r<N>_p<fn> per function.
func scanInventory(profile *v1alpha1.NodePrepProfile) (nics []v1alpha1.NicStatus, gpus []v1alpha1.GpuStatus, mellanox []pciDevice, err error) {
	entries, err := os.ReadDir("/sys/bus/pci/devices")
	if err != nil {
		return nil, nil, nil, err
	}
	railByFn := map[string]string{}
	for _, r := range profile.Spec.Rails {
		railByFn[r.PCIFunction] = r.Rail
	}

	for _, e := range entries {
		pci := e.Name()
		base := "/sys/bus/pci/devices/" + pci
		vendor, err := readSysfsString(base + "/vendor")
		if err != nil {
			continue
		}
		class, _ := readSysfsString(base + "/class")

		switch vendor {
		case "0x10de": // NVIDIA
			if !strings.HasPrefix(class, "0x03") {
				continue
			}
			g := v1alpha1.GpuStatus{
				PCI:       pci,
				Name:      fmt.Sprintf("NVIDIA GPU (device %s)", mustDevice(base)),
				LinkWidth: widthOf(base),
				LinkSpeed: speedOf(base),
			}
			gpus = append(gpus, g)
		case "0x15b3": // Mellanox / NVIDIA Networking
			fn := pciToFn(pci)
			d := pciDevice{
				pci:       pci,
				fn:        fn,
				vendor:    vendor,
				deviceID:  mustDevice(base),
				class:     class,
				linkWidth: widthOf(base),
				linkSpeed: speedOf(base),
			}
			d.netdev = firstNetdev(base)
			mellanox = append(mellanox, d)
		}
	}

	// Rail assignment, second pass so multi-function cards (two ports on
	// one PCI device) get the bash r<N>_p<port> grammar per function.
	fnCount := map[string]int{}
	for _, d := range mellanox {
		fnCount[d.fn]++
	}
	for i := range mellanox {
		d := &mellanox[i]
		base := railByFn[d.fn]
		if base == "" {
			continue
		}
		if fnCount[d.fn] > 1 {
			d.rail = base + "_p" + railPort(d.pci)
		} else {
			d.rail = base
		}
	}

	for _, d := range mellanox {
		nics = append(nics, d.nicStatus())
	}
	return nics, gpus, mellanox, nil
}

// nicStatus renders the status-view of one function.
func (d pciDevice) nicStatus() v1alpha1.NicStatus {
	t := d.devType
	if t == "" {
		t = "Mellanox" // classification requires MFT; retried every refresh
	}
	return v1alpha1.NicStatus{
		PCI:       d.pci,
		Fn:        d.fn,
		Type:      t,
		Variant:   d.variant,
		Firmware:  d.fwVer,
		PSID:      d.psid,
		Rail:      railLabel(d),
		Rshim:     d.rshim,
		NetDev:    d.netdev,
		IBDev:     d.ibdev,
		LinkWidth: d.linkWidth,
		LinkSpeed: d.linkSpeed,
		DeviceID:  d.deviceID,
	}
}

// railLabel renders the rail label the bash script would have produced:
// mapped rail name, "dpu" for DPUs, empty when unassigned.
func railLabel(d pciDevice) string {
	if d.rail != "" {
		return d.rail
	}
	if d.isDPU() || strings.Contains(strings.ToLower(d.rawType), "bluefield") {
		return "dpu"
	}
	return ""
}

// enrichMellanox fills the MFT-derived fields of every Mellanox function:
// device classification from the mlxconfig device header, firmware/PSID via
// flint, the rshim device (DPUs) and the sysfs IB device name. One full
// `mlxconfig q` per PCI address classifies it — mlxconfig prints the
// Description and Device type header on every query, whether or not the
// device supports the probed key (the bash reads the same header from its
// INTERNAL_CPU_OFFLOAD_ENGINE probe without checking the probe's exit
// code). mlxconfig/flint run once per PCI address per agent process — the
// values only change on firmware operations — and failures are retried on
// later refreshes so a classification that ran before MFT was installed
// corrects itself.
//
// MFT missing is the expected state on a fresh node (the aptPackages step
// installs it, later in the same Provisioning stage): the pass detects that
// once via findHostTool, skips the per-device exec attempts, and logs a
// single notice per process instead of a per-device "not classified" line
// every cycle (found noisy live on bl-r1-c2-02). The pending state stays
// visible in the NodePrep status — unclassified functions report
// Type=Mellanox until classified. A mlxconfig that IS installed but fails
// per device is still a per-device error line.
func (a *Agent) enrichMellanox(mellanox []pciDevice) {
	_, mftMissing := findHostTool("mlxconfig")
	pending := 0
	for i := range mellanox {
		d := &mellanox[i]
		if c, ok := a.mftCache[d.pci]; ok {
			d.devType, d.rawType, d.variant = c.devType, c.rawType, c.variant
			d.fwVer, d.psid = c.fwVer, c.psid
			d.ibdev = ibdevFor(d.pci)
			d.rshim = rshimFor(d.fn)
			continue
		}
		d.ibdev = ibdevFor(d.pci)
		d.rshim = rshimFor(d.fn)
		if mftMissing != nil {
			pending++
			continue // classification retries once MFT is installed
		}
		// Both tool dumps run quiet (the one-line classification log below
		// is the record; -verbose shows the full output): mlxconfig prints
		// the device's whole configuration table, flint the whole image
		// info block.
		if out, err := a.hostExecQuiet(nil, 60*time.Second, "mlxconfig", "-d", d.pci, "q"); err == nil {
			d.rawType = rawDeviceType(out)
			d.devType, d.variant = classifyMlxconfig(out)
		} else {
			a.logf("inventory: %s not classified (mlxconfig query failed: %v)", d.pci, err)
			continue // classification retries on a later refresh
		}
		a.logf("inventory: %s (%s) classified %s/%s fw %s psid %s rshim %s", d.pci, d.netdev, d.devType, d.variant, d.fwVer, d.psid, d.rshim)
		if out, err := a.hostExecQuiet(nil, 60*time.Second, "flint", "-d", d.pci, "q"); err == nil {
			d.fwVer, d.psid = parseFlint(out)
		}
		a.mftCache[d.pci] = mftInfo{devType: d.devType, rawType: d.rawType, variant: d.variant, fwVer: d.fwVer, psid: d.psid}
	}
	if pending > 0 && !a.mftPendingLogged {
		a.mftPendingLogged = true
		a.logf("inventory: mlxconfig (MFT) not installed yet; %d function(s) pending classification — the aptPackages step installs it and the classification lands on a later refresh", pending)
	}
}

// mftInfo is the cached per-function MFT data.
type mftInfo struct {
	devType string
	rawType string
	variant string
	fwVer   string
	psid    string
}

// classifyMlxconfig mirrors the bash fn_inventory_hw classification. The
// Description line decides the variant: every real device carries an actual
// description, so "N/A" means the device is emulated (NVIDIA DSX Air) —
// variant Air — and the Device type line is read instead. Physical
// classification uses the description, the only place a BlueField-3
// SuperNIC ("…HHHL SuperNIC; …") differs from a BlueField-3 DPU ("…DPU;
// …"): both report Device type "BlueField3". ConnectX devices take the
// bash family form (text between ':' and ';', trimmed, spaces →
// underscores); anything else is Unknown — a DSX Air BlueField-3 included,
// exactly as the bash classifies it.
func classifyMlxconfig(out string) (devType, variant string) {
	descLine, typeLine := "", ""
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "Description:"):
			descLine = ln
		case strings.HasPrefix(ln, "Device type:"):
			typeLine = ln
		}
	}
	line := descLine
	variant = "Physical"
	if f := strings.Fields(descLine); len(f) > 1 && f[1] == "N/A" {
		line = typeLine
		variant = "Air"
	}
	switch {
	case strings.Contains(line, "SuperNIC"):
		return "SuperNIC", variant
	case strings.Contains(line, "DPU"):
		return "DPU", variant
	case strings.Contains(line, "ConnectX"):
		// bash: text between ':' and ';', trimmed, spaces -> underscores
		s := line[strings.Index(line, ":")+1:]
		if i := strings.Index(s, ";"); i >= 0 {
			s = s[:i]
		}
		return strings.ReplaceAll(strings.TrimSpace(s), " ", "_"), variant
	}
	return "Unknown", variant
}

// rawDeviceType extracts the literal "Device type:" value (ConnectX4Lx,
// BlueField3, ...) used by model-specific gates.
func rawDeviceType(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "Device type:") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "Device type:"))
		}
	}
	return ""
}

// parseFlint pulls "FW Version:" and "PSID:" out of a flint query.
func parseFlint(out string) (fwVer, psid string) {
	for _, ln := range strings.Split(out, "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		if strings.HasPrefix(ln, "FW Version:") && len(fields) >= 3 {
			fwVer = fields[2]
		}
		if strings.HasPrefix(ln, "PSID:") {
			psid = fields[1]
		}
	}
	return fwVer, psid
}

// ibdevFor returns the first IB device of the PCI function from sysfs.
func ibdevFor(pci string) string {
	ents, err := os.ReadDir("/sys/bus/pci/devices/" + pci + "/infiniband")
	if err != nil || len(ents) == 0 {
		return ""
	}
	return ents[0].Name()
}

// rshimFor finds the rshim device owning this function (bash: /dev/rshim*/misc
// DEV_NAME "pcie-<bus>:<dev>.<fn>" compared up to the first dot).
func rshimFor(fn string) string {
	want := "pcie-0000:" + fn
	ents, err := os.ReadDir("/dev")
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "rshim") {
			continue
		}
		b, err := os.ReadFile("/dev/" + e.Name() + "/misc")
		if err != nil {
			continue
		}
		for _, ln := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(ln, "DEV_NAME") {
				continue
			}
			fields := strings.Fields(ln)
			if len(fields) < 2 {
				continue
			}
			if name := strings.SplitN(fields[1], ".", 2)[0]; name == want {
				return "/dev/" + e.Name()
			}
		}
	}
	return ""
}

func pciToFn(pci string) string {
	// 0000:05:00.0 -> 05:00
	parts := strings.Split(pci, ":")
	if len(parts) != 3 {
		return ""
	}
	return parts[1] + ":" + strings.Split(parts[2], ".")[0]
}

func firstNetdev(base string) string {
	dir := base + "/net"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if phys, err := readSysfsString(dir + "/" + name + "/phys_port_name"); err == nil && strings.Contains(phys, "vf") {
			continue // skip VF interfaces (design §8.1 netdev selection)
		}
		return name
	}
	return ""
}

func readSysfsString(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func mustDevice(base string) string {
	d, _ := readSysfsString(base + "/device")
	return d
}

func widthOf(base string) string {
	w, err := readSysfsString(base + "/current_link_width")
	if err != nil || w == "" {
		return ""
	}
	return w + "x"
}

func speedOf(base string) string {
	s, err := readSysfsString(base + "/max_link_speed")
	if err != nil || s == "" {
		return ""
	}
	// sysfs reports e.g. "32.0 GT/s PCIe" — keep the numeric part.
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimSuffix(fields[0], ".0") + "GTs"
}
