package phases

import (
	"testing"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

func TestFromLegacy(t *testing.T) {
	cases := map[string]v1alpha1.Phase{
		"":            v1alpha1.PhasePending,
		"init":        v1alpha1.PhaseProvisioning,
		"inithw":      v1alpha1.PhaseFlashing,
		"config":      v1alpha1.PhaseConfiguring,
		"precomplete": v1alpha1.PhaseFinalizing,
		"complete":    v1alpha1.PhaseFinalizing, // import re-verifies via Detect (design §10)
	}
	for in, want := range cases {
		got, err := FromLegacy(in)
		if err != nil || got != want {
			t.Errorf("FromLegacy(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := FromLegacy("bogus"); err == nil {
		t.Error("FromLegacy(bogus) should error")
	}
}

func TestLegacyMirror(t *testing.T) {
	// LegacyFor mirrors live phases; FromLegacy imports bash state. The two
	// are deliberately not symmetric for "complete" (import re-verifies), so
	// only the live mirror direction is round-trip tested here.
	for _, p := range []v1alpha1.Phase{
		v1alpha1.PhaseProvisioning, v1alpha1.PhaseFlashing,
		v1alpha1.PhaseConfiguring, v1alpha1.PhaseFinalizing,
	} {
		v, err := LegacyFor(p)
		if err != nil {
			t.Fatalf("LegacyFor(%v): %v", p, err)
		}
		back, err := FromLegacy(v)
		if err != nil || back != p {
			t.Errorf("round trip %v -> %q -> %v (%v)", p, v, back, err)
		}
	}
	if v, _ := LegacyFor(v1alpha1.PhaseReady); v != "complete" {
		t.Error("Ready must mirror to complete")
	}
	// The cold overlay parks the precomplete stage: it mirrors the label a
	// bash-era consumer expects, and deliberately does NOT round-trip
	// (FromLegacy("precomplete") is Finalizing — the import re-verifies).
	if v, err := LegacyFor(v1alpha1.PhaseColdRebootRequired); err != nil || v != "precomplete" {
		t.Errorf("LegacyFor(ColdRebootRequired) = %q, %v; want precomplete", v, err)
	}
}

func TestAtLeast(t *testing.T) {
	if !AtLeast(v1alpha1.PhaseFinalizing, v1alpha1.PhaseFlashing) {
		t.Error("Finalizing should be at least Flashing")
	}
	if AtLeast(v1alpha1.PhaseProvisioning, v1alpha1.PhaseReady) {
		t.Error("Provisioning is not at least Ready")
	}
	if AtLeast(v1alpha1.PhaseFailed, v1alpha1.PhasePending) {
		t.Error("Failed is never at least anything")
	}
	if !AtLeast(v1alpha1.PhaseColdRebootRequired, v1alpha1.PhaseFinalizing) {
		t.Error("the cold overlay parks Finalizing, so it is at least Finalizing")
	}
	if !AtLeast(v1alpha1.PhaseReady, v1alpha1.PhaseColdRebootRequired) {
		t.Error("Ready is past the cold overlay")
	}
	if AtLeast(v1alpha1.PhaseColdRebootRequired, v1alpha1.PhaseReady) {
		t.Error("the cold overlay is not at Ready")
	}
}
