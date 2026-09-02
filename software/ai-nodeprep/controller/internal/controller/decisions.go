package controller

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// AdmitControlPlane implements the quorum admission window (design §6.4):
// at most CPMaxConcurrent(expected) control-plane nodes mid-prep at once.
// Background strategy (single-CP clusters) admits unconditionally — the
// work must happen even though the math allows zero disruptions.
func AdmitControlPlane(busyOthers, maxConcurrent int, strategy string) bool {
	if strategy == "background" {
		return true
	}
	return busyOthers < maxConcurrent
}

// BusyControlPlane reports whether a control-plane NodePrep consumes the
// §6.4 maintenance window: any stage past Provisioning (the host is being
// worked on), or an admission already granted while still queued. Waiting
// peers (MaintenanceAdmitted=False in Pending/Provisioning) do not count —
// counting them deadlocks the window, since every member would wait on
// every other.
func BusyControlPlane(phase v1alpha1.Phase, maintenanceAdmitted string) bool {
	switch phase {
	case v1alpha1.PhaseFlashing, v1alpha1.PhaseConfiguring, v1alpha1.PhaseFinalizing:
		return true
	case v1alpha1.PhasePending, v1alpha1.PhaseProvisioning:
		return maintenanceAdmitted == string(metav1.ConditionTrue)
	default: // Ready, Failed, "" — converged or not started
		return false
	}
}

// CPMaxConcurrent computes how many control-plane nodes may be mid-prep
// concurrently: expected − quorum(expected) — 1 for a 3-node CP, 2 for
// 5 — with a floor of 1 so prep always makes progress (a 2-node CP is
// pathological either way; document, don't deadlock).
func CPMaxConcurrent(expected int) int {
	if expected < 1 {
		return 1
	}
	quorum := expected/2 + 1
	n := expected - quorum
	if n < 1 {
		return 1
	}
	return n
}

// ExcludeLabelMatch reports whether a node is disqualified by the profile's
// excludeLabel ("key" matches any value, "key=value" an exact one).
func ExcludeLabelMatch(exclude string, labels map[string]string) bool {
	if exclude == "" {
		return false
	}
	key, want, hasVal := strings.Cut(exclude, "=")
	got, ok := labels[key]
	if !ok {
		return false
	}
	return !hasVal || got == want
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
