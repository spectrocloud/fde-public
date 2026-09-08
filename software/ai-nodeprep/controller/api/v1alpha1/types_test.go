package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

// The eastWest multi-plane/breakout reserves (0.1.77): absent fields read
// as the schema default 1, explicit values pass through, and the JSON names
// are the camelCase forms the CRD schema carries.
func TestEastWestPlanesAndBreakoutDefaults(t *testing.T) {
	var e EastWestSpec
	if e.PlanesNumOrDefault() != 1 || e.NICBreakoutOrDefault() != 1 {
		t.Fatalf("absent fields must default to 1: planes=%d breakout=%d",
			e.PlanesNumOrDefault(), e.NICBreakoutOrDefault())
	}
	e.PlanesNum = 2
	e.NICBreakout = 4
	if e.PlanesNumOrDefault() != 2 || e.NICBreakoutOrDefault() != 4 {
		t.Fatalf("explicit values must pass through: planes=%d breakout=%d",
			e.PlanesNumOrDefault(), e.NICBreakoutOrDefault())
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"planesNum":2`, `"nicBreakout":4`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("marshalled eastWest %s missing %s (JSON names must match the CRD schema)", b, want)
		}
	}
}
