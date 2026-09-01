package controller

import (
	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// Pure admission/taint decision functions, unit-tested; the reconcile loop
// only applies their verdicts.

// TaintShouldExist implements the taint contract (design §6.1): the taint is
// present while the node is owned by nodeprep, and released only when the
// phase is Ready AND the agent has verified the current boot. Failed nodes
// keep the taint.
func TaintShouldExist(phase v1alpha1.Phase, bootVerified bool, policy v1alpha1.PolicySpec) bool {
	if !policy.TaintsOn() {
		return false
	}
	return !(phase == v1alpha1.PhaseReady && bootVerified)
}

// AdmitFlashing is the fleet flash window (design §9.1): at most max nodes
// flash concurrently (max <= 0 means unlimited in v0.1).
func AdmitFlashing(busyOthers, max int) bool {
	return max <= 0 || busyOthers < max
}

// AdmitControlPlane implements serial control-plane admission (design §6.4,
// quorum-safe strategy): at most one control-plane node may be mid-prep.
// Background strategy (single-CP clusters) admits unconditionally.
func AdmitControlPlane(busyOthers int, strategy string) bool {
	if strategy == "background" {
		return true
	}
	return busyOthers == 0
}

// WorkerLabelOp is the tri-state decision for the worker-role label.
type WorkerLabelOp int

const (
	WorkerLabelNone WorkerLabelOp = iota
	WorkerLabelSet
	WorkerLabelRemove
)

// WorkerLabelDecision implements design §6.3: demoted entering Finalizing,
// restored at Ready, untouched otherwise. Only when policy says manage
// (empty means manage, matching the legacy controller's unconditional
// behavior).
func WorkerLabelDecision(phase v1alpha1.Phase, policy v1alpha1.PolicySpec) WorkerLabelOp {
	if policy.WorkerRoleLabel != "" && policy.WorkerRoleLabel != "manage" {
		return WorkerLabelNone
	}
	switch phase {
	case v1alpha1.PhaseFinalizing:
		return WorkerLabelRemove
	case v1alpha1.PhaseReady:
		return WorkerLabelSet
	default:
		return WorkerLabelNone
	}
}
