package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// pciDevice is one enumerated PCI function of interest.
type pciDevice struct {
	pci       string // 0000:05:00.0
	fn        string // 05:00 (bash rail-key form)
	vendor    string // 0x15b3, 0x10de
	deviceID  string
	class     string
	rail      string // assigned rail (r0, dpu, ...) or ""
	linkWidth string
	linkSpeed string
	netdev    string
}

// scanInventory walks /sys/bus/pci/devices and classifies NVIDIA GPUs and
// Mellanox NICs from sysfs alone (design §8.4: no lspci fork). Mellanox
// classification beyond "Mellanox" (SuperNIC/DPU/ConnectX via mlxconfig
// INTERNAL_CPU_OFFLOAD_ENGINE) requires MFT and lands in v0.2.
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
				rail:      railByFn[fn],
				linkWidth: widthOf(base),
				linkSpeed: speedOf(base),
			}
			d.netdev = firstNetdev(base)
			mellanox = append(mellanox, d)
			nics = append(nics, v1alpha1.NicStatus{
				PCI:       pci,
				Fn:        fn,
				Type:      "Mellanox", // classification via mlxconfig lands in v0.2 (design §8.1)
				DeviceID:  d.deviceID,
				Rail:      railLabel(d),
				NetDev:    d.netdev,
				LinkWidth: d.linkWidth,
				LinkSpeed: d.linkSpeed,
			})
		}
	}
	return nics, gpus, mellanox, nil
}

// railLabel renders the rail label the bash script would have produced:
// mapped rail name, "dpu" for DPUs, empty when unassigned.
func railLabel(d pciDevice) string {
	if d.rail != "" {
		return d.rail
	}
	if d.fn != "" && strings.HasSuffix(d.fn, ".0") {
		// Without MFT we cannot tell DPU from NIC; leave empty and let the
		// profile rails drive rail assignment.
		return ""
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

var _ = filepath.Join
