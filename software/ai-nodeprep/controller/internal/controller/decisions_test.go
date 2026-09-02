package controller

import (
	"testing"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

func TestTaintShouldExist(t *testing.T) {
	pol := v1alpha1.PolicySpec{}
	cases := []struct {
		phase        v1alpha1.Phase
		bootVerified bool
		want         bool
	}{
		{v1alpha1.PhasePending, false, true},
		{v1alpha1.PhaseProvisioning, false, true},
		{v1alpha1.PhaseFlashing, false, true},
		{v1alpha1.PhaseFinalizing, false, true},
		{v1alpha1.PhaseReady, false, true},  // Ready but boot not verified yet
		{v1alpha1.PhaseReady, true, false},  // the only release path (design §6.1)
		{v1alpha1.PhaseFailed, false, true}, // Failed keeps the window held
		{v1alpha1.PhaseFailed, true, true},  // even a stale Verified flag must not release
	}
	for _, tc := range cases {
		if got := TaintShouldExist(tc.phase, tc.bootVerified, pol); got != tc.want {
			t.Errorf("TaintShouldExist(%v, %v) = %v, want %v", tc.phase, tc.bootVerified, got, tc.want)
		}
	}
	off := false
	polOff := v1alpha1.PolicySpec{TaintEnabled: &off}
	if TaintShouldExist(v1alpha1.PhasePending, false, polOff) {
		t.Error("taintEnabled=false must disable the taint entirely")
	}
}

func TestAdmitFlashing(t *testing.T) {
	if !AdmitFlashing(3, 0) {
		t.Error("max<=0 must mean unlimited")
	}
	if !AdmitFlashing(1, 2) {
		t.Error("1 busy of max 2 must admit")
	}
	if AdmitFlashing(2, 2) {
		t.Error("2 busy of max 2 must not admit")
	}
}

func TestAdmitControlPlane(t *testing.T) {
	if !AdmitControlPlane(1, "background") {
		t.Error("background strategy (single-CP) must always admit")
	}
	if !AdmitControlPlane(0, "") {
		t.Error("serial strategy with no busy peers must admit")
	}
	if AdmitControlPlane(1, "serial") {
		t.Error("serial strategy with a busy peer must hold")
	}
}

func TestWorkerLabelDecision(t *testing.T) {
	pol := v1alpha1.PolicySpec{}
	if WorkerLabelDecision(v1alpha1.PhaseFinalizing, pol) != WorkerLabelRemove {
		t.Error("Finalizing must remove the worker label (design §6.3)")
	}
	if WorkerLabelDecision(v1alpha1.PhaseReady, pol) != WorkerLabelSet {
		t.Error("Ready must restore the worker label")
	}
	for _, p := range []v1alpha1.Phase{v1alpha1.PhasePending, v1alpha1.PhaseProvisioning, v1alpha1.PhaseFlashing, v1alpha1.PhaseConfiguring, v1alpha1.PhaseFailed} {
		if WorkerLabelDecision(p, pol) != WorkerLabelNone {
			t.Errorf("phase %v must not touch the worker label", p)
		}
	}
	ignore := v1alpha1.PolicySpec{WorkerRoleLabel: "ignore"}
	if WorkerLabelDecision(v1alpha1.PhaseReady, ignore) != WorkerLabelNone {
		t.Error("workerRoleLabel=ignore must disable management")
	}
}
