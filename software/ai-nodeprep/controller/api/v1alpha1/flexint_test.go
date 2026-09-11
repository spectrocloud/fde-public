package v1alpha1

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Palette-deployed profiles carry integers that pack templating turned into
// strings (Kevin: "you get the equivalent of spec.eastWest.numVFs: \"0\"
// getting applied, which results in an error as the CRD does not allow a
// string input"). The spec structs must normalize numeric strings at decode
// and keep marshaling ints.
func TestFlexIntStringFieldsDecode(t *testing.T) {
	in := `{"spec": {
		"eastWest":  {"numVFs": "16", "mtu": "9000", "planesNum": "1", "nicBreakout": "2"},
		"northSouth": {"numVFs": "0"},
		"hostBoot":  {"hugepages": {"pages1G": "0", "pages2M": "5120"}},
		"policy":    {"maxConcurrentFlashes": "4"},
		"controlPlane": {"expectedCount": "3"}
	}}`
	p := &NodePrepProfile{}
	if err := json.Unmarshal([]byte(in), p); err != nil {
		t.Fatalf("numeric-string profile must decode: %v", err)
	}
	if p.Spec.EastWest.NumVFs != 16 || p.Spec.EastWest.MTU != 9000 ||
		p.Spec.EastWest.PlanesNum != 1 || p.Spec.EastWest.NICBreakout != 2 {
		t.Fatalf("eastWest not normalized: %+v", p.Spec.EastWest)
	}
	if p.Spec.NorthSouth.NumVFs != 0 {
		t.Fatalf("northSouth not normalized: %+v", p.Spec.NorthSouth)
	}
	if p.Spec.HostBoot.Hugepages.Pages1G != 0 || p.Spec.HostBoot.Hugepages.Pages2M != 5120 {
		t.Fatalf("hugepages not normalized: %+v", p.Spec.HostBoot.Hugepages)
	}
	if p.Spec.Policy.MaxConcurrentFlashes != 4 || p.Spec.ControlPlane.ExpectedCount != 3 {
		t.Fatalf("policy/controlPlane not normalized: %d %d",
			p.Spec.Policy.MaxConcurrentFlashes, p.Spec.ControlPlane.ExpectedCount)
	}

	// Plain numbers still decode, and marshaling emits integers — the stored
	// form and every status readback stay int-shaped.
	p2 := &NodePrepProfile{}
	if err := json.Unmarshal([]byte(`{"spec": {"eastWest": {"numVFs": 16, "mtu": 9000}}}`), p2); err != nil {
		t.Fatalf("numeric profile must decode: %v", err)
	}
	out, err := json.Marshal(p2)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"numVFs":16`, `"mtu":9000`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("marshal must emit integers (%s), got %s", want, out)
		}
	}
	if strings.Contains(string(out), `"numVFs":"`) {
		t.Fatalf("marshal must never emit a numeric string, got %s", out)
	}

	// null and absent fields are the zero value, not errors.
	p3 := &NodePrepProfile{}
	if err := json.Unmarshal([]byte(`{"spec": {"eastWest": {"numVFs": null}}}`), p3); err != nil {
		t.Fatalf("null must decode as zero: %v", err)
	}
	if p3.Spec.EastWest.NumVFs != 0 {
		t.Fatalf("null numVFs must stay zero, got %d", p3.Spec.EastWest.NumVFs)
	}

	// A non-numeric string is an error naming the field — caught at decode
	// even if a lenient CRD ever lets it through.
	p4 := &NodePrepProfile{}
	err = json.Unmarshal([]byte(`{"spec": {"eastWest": {"numVFs": "sixteen"}}}`), p4)
	if err == nil || !strings.Contains(err.Error(), `field "numVFs"`) {
		t.Fatalf("bad string must fail naming the field, got %v", err)
	}
}

// offloadEngine became a boolean in 0.1.84, but the same pack-templating
// mechanism that stringifies integers renders it as a string, and profiles
// written before the flip still carry the legacy enum in stored objects.
// The decode must normalize all of it (sf/smf → true, none → false) and
// marshal real booleans.
func TestFlexOffloadEngineDecodes(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`true`, true}, {`false`, false}, // native booleans pass through
		{`"true"`, true}, {`"True"`, true}, {`"1"`, true}, // templated strings
		{`"sf"`, true}, {`"smf"`, true}, // legacy: active offload engine
		{`"false"`, false}, {`"0"`, false}, // templated strings
		{`"none"`, false}, {`""`, false}, // legacy: no offload engine
	}
	for _, c := range cases {
		p := &NodePrepProfile{}
		in := fmt.Sprintf(`{"spec": {"northSouth": {"offloadEngine": %s}}}`, c.in)
		if err := json.Unmarshal([]byte(in), p); err != nil {
			t.Fatalf("offloadEngine %s must decode: %v", c.in, err)
		}
		if p.Spec.NorthSouth.OffloadEngine != c.want {
			t.Errorf("offloadEngine %s: got %v, want %v", c.in, p.Spec.NorthSouth.OffloadEngine, c.want)
		}
	}

	// true marshals as a real boolean; false is omitempty-dropped.
	p := &NodePrepProfile{}
	if err := json.Unmarshal([]byte(`{"spec": {"northSouth": {"offloadEngine": "sf"}}}`), p); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"offloadEngine":true`) || strings.Contains(string(out), `"offloadEngine":"`) {
		t.Fatalf("marshal must emit a boolean, got %s", out)
	}

	// Anything outside the known domain is an error naming the field.
	p2 := &NodePrepProfile{}
	err = json.Unmarshal([]byte(`{"spec": {"northSouth": {"offloadEngine": "maybe"}}}`), p2)
	if err == nil || !strings.Contains(err.Error(), `field "offloadEngine"`) {
		t.Fatalf("unknown string must fail naming the field, got %v", err)
	}
}

func TestFlexIntWhitespaceTolerated(t *testing.T) {
	p := &NodePrepProfile{}
	if err := json.Unmarshal([]byte(`{"spec": {"eastWest": {"numVFs": " 16 "}}}`), p); err != nil {
		t.Fatalf("padded numeric string must decode: %v", err)
	}
	if p.Spec.EastWest.NumVFs != 16 {
		t.Fatalf("padded string not normalized: %d", p.Spec.EastWest.NumVFs)
	}
}
