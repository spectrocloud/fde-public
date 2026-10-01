// Package phases maps between the controller's Phase model and the bash
// script's legacy spectrocloud.com/nodeprep label values (design §5.2, §10.1).
package phases

import (
	"fmt"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

var order = map[v1alpha1.Phase]float64{
	v1alpha1.PhasePending:      0,
	v1alpha1.PhaseProvisioning: 1,
	v1alpha1.PhaseFlashing:     2,
	v1alpha1.PhaseConfiguring:  3,
	v1alpha1.PhaseFinalizing:   4,
	// The cold overlay parks the Finalizing walk: past Finalizing's own
	// convergence point but never at Ready (0.1.54).
	v1alpha1.PhaseColdRebootRequired: 4.5,
	v1alpha1.PhaseReady:              5,
	v1alpha1.PhaseFailed:             -1,
}

// AtLeast reports whether p is at or past other in the bring-up ordering.
// Failed is never "at least" anything.
func AtLeast(p, other v1alpha1.Phase) bool {
	return order[p] >= order[other] && p != v1alpha1.PhaseFailed
}

// LegacyFor maps a Phase onto the bash label value with the same meaning.
// Provisioning→init, Flashing→inithw, Configuring→config,
// Finalizing→precomplete, Ready→complete. Pending and Failed have no legacy
// value; mirroring keeps the last known value instead.
func LegacyFor(p v1alpha1.Phase) (string, error) {
	switch p {
	case v1alpha1.PhasePending, v1alpha1.PhaseProvisioning:
		return "init", nil
	case v1alpha1.PhaseFlashing:
		return "inithw", nil
	case v1alpha1.PhaseConfiguring:
		return "config", nil
	case v1alpha1.PhaseFinalizing:
		return "precomplete", nil
	case v1alpha1.PhaseColdRebootRequired:
		// The overlay parks the precomplete stage; the legacy label keeps
		// the value a bash-era consumer expects while the walk is halted.
		return "precomplete", nil
	case v1alpha1.PhaseReady:
		return "complete", nil
	default:
		return "", fmt.Errorf("no legacy value for phase %q", p)
	}
}

// FromLegacy maps a bash label value onto the equivalent Phase, per the
// import mapping in the design (§10 rollout). An empty value maps to Pending.
func FromLegacy(v string) (v1alpha1.Phase, error) {
	switch v {
	case "":
		return v1alpha1.PhasePending, nil
	case "init":
		return v1alpha1.PhaseProvisioning, nil
	case "inithw":
		return v1alpha1.PhaseFlashing, nil
	case "config":
		return v1alpha1.PhaseConfiguring, nil
	case "precomplete", "complete":
		return v1alpha1.PhaseFinalizing, nil
	default:
		return "", fmt.Errorf("unknown legacy nodeprep value %q", v)
	}
}

// Stages lists the bring-up phases in execution order.
func Stages() []v1alpha1.Phase {
	return []v1alpha1.Phase{
		v1alpha1.PhaseProvisioning,
		v1alpha1.PhaseFlashing,
		v1alpha1.PhaseConfiguring,
		v1alpha1.PhaseFinalizing,
	}
}
