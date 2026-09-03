package agent

import (
	"strings"
	"testing"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

func TestVfIdentity(t *testing.T) {
	// Live rail PF GUID (bl-r1-c2-05, roce_r0_p0) — formulas are bash
	// fn_set_vfs L675-686: node f1, port f2, mac f2+vf+tail, all
	// colon-stripped.
	const guid = "905a0815d91d2edc"
	cases := []struct {
		vf              int
		node, port, mac string
	}{
		{0, "905a081f100d2edc", "905a081f200d2edc", "f200d91d2edc"},
		{1, "905a081f101d2edc", "905a081f201d2edc", "f201d91d2edc"},
		{3, "905a081f103d2edc", "905a081f203d2edc", "f203d91d2edc"},
		{15, "905a081f10fd2edc", "905a081f20fd2edc", "f20fd91d2edc"},
	}
	for _, c := range cases {
		node, port, mac, err := vfIdentity(guid, c.vf)
		if err != nil {
			t.Fatalf("vf%d: %v", c.vf, err)
		}
		if node != c.node || port != c.port || mac != c.mac {
			t.Errorf("vf%d: got node=%s port=%s mac=%s, want %s/%s/%s",
				c.vf, node, port, mac, c.node, c.port, c.mac)
		}
	}
}

func TestVfIdentityRejectsBadGUID(t *testing.T) {
	for _, bad := range []string{"", "905a081", "zz5a0815d91d2edc", "905a0815d91d2edcff"} {
		if _, _, _, err := vfIdentity(bad, 0); err == nil {
			t.Errorf("vfIdentity(%q) should fail", bad)
		}
	}
}

func TestRenderGrubDropin(t *testing.T) {
	got := renderGrubDropin([]string{"intel_iommu=on", "iommu=pt"})
	want := `GRUB_CMDLINE_LINUX=" $GRUB_CMDLINE_LINUX "
GRUB_CMDLINE_LINUX="$(printf '%s' "$GRUB_CMDLINE_LINUX" | sed -E 's/(^|[[:space:]])intel_iommu=[^[:space:]]*//g;s/(^|[[:space:]])iommu=[^[:space:]]*//g')"
GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX intel_iommu=on iommu=pt"
GRUB_CMDLINE_LINUX="$(echo "$GRUB_CMDLINE_LINUX" | xargs)"
`
	if got != want {
		t.Errorf("drop-in mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
	// Single key: no separator after the one sed expression.
	got = renderGrubDropin([]string{"iommu=pt"})
	if !strings.Contains(got, `sed -E 's/(^|[[:space:]])iommu=[^[:space:]]*//g')`) {
		t.Errorf("single-key sed terminator wrong:\n%s", got)
	}
}

func TestGrubLineHasParam(t *testing.T) {
	yes := []string{
		`GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX iommu=pt"`,
		`iommu=pt quiet`,
		`GRUB_CMDLINE_LINUX="iommu=pt"`,
		`x iommu=pt`,
	}
	for _, line := range yes {
		if !grubLineHasParam(line, "iommu=pt") {
			t.Errorf("should match: %q", line)
		}
	}
	no := []string{
		`GRUB_CMDLINE_LINUX="noiommu=pt"`,
		`xiommu=pt`,
		`GRUB_CMDLINE_LINUX="iommu=ptrue"`,
		`GRUB_CMDLINE_LINUX="iommu=off"`,
		``,
	}
	for _, line := range no {
		if grubLineHasParam(line, "iommu=pt") {
			t.Errorf("should not match: %q", line)
		}
	}
}

func TestAppendUnique(t *testing.T) {
	got := appendUnique([]string{"a"}, "b")
	got = appendUnique(got, "a")
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("got %v", got)
	}
}

func TestColonFormat(t *testing.T) {
	cases := map[string]string{
		"905a081f100d2edc": "90:5a:08:1f:10:0d:2e:dc",
		"f200d91d2edc":     "f2:00:d9:1d:2e:dc",
		"":                 "",
	}
	for in, want := range cases {
		if got := colonFormat(in); got != want {
			t.Errorf("colonFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVfCountFor(t *testing.T) {
	prof := &v1alpha1.NodePrepProfile{}
	prof.Spec.EastWest.NumVFs = 4
	prof.Spec.NorthSouth.NumVFs = 2
	cases := []struct {
		rail string
		want int
	}{
		{"r0_p0", 4}, // rail-mapped: east-west
		{"r1_p0", 4}, // any rail-mapped function
		{"dpu", 2},   // DPU: north-south
		{"", 0},      // not rail-mapped: NO east-west VFs (bond0 uplink etc.)
	}
	for _, c := range cases {
		if got := vfCountFor(prof, pciDevice{rail: c.rail}); got != c.want {
			t.Errorf("rail %q: got %d, want %d", c.rail, got, c.want)
		}
	}
}

func TestAggregateReboots(t *testing.T) {
	// Single request passes through unchanged.
	r, m := aggregateReboots([]rebootRequest{{reason: "GrubChanged", message: "cmdline updated"}})
	if r != "GrubChanged" || m != "cmdline updated" {
		t.Errorf("single: got %q/%q", r, m)
	}
	// Multiple requests: first reason is the token, all reasons and
	// messages name themselves in the message (the fresh-node shape:
	// doca + grub + ib_core in one boot).
	r, m = aggregateReboots([]rebootRequest{
		{reason: "DocaInstalled", message: "DOCA installed"},
		{reason: "GrubChanged", message: "cmdline updated"},
		{reason: "IbCoreNetns", message: "netns_mode"},
	})
	if r != "DocaInstalled" {
		t.Errorf("multi reason: got %q", r)
	}
	for _, want := range []string{"DocaInstalled+GrubChanged+IbCoreNetns", "DOCA installed", "cmdline updated", "netns_mode"} {
		if !strings.Contains(m, want) {
			t.Errorf("multi message missing %q:\n%s", want, m)
		}
	}
}
