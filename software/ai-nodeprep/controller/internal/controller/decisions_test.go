package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	if !AdmitControlPlane(1, 1, "background") {
		t.Error("background strategy (single-CP) must always admit")
	}
	if !AdmitControlPlane(0, 1, "") {
		t.Error("serial strategy with no busy peers must admit")
	}
	if AdmitControlPlane(1, 1, "serial") {
		t.Error("serial strategy at the window cap must hold")
	}
	if !AdmitControlPlane(1, 2, "serial") {
		t.Error("a wider window admits below the cap")
	}
}

// CPMaxConcurrent implements the §6.4 quorum math: expected − quorum, so a
// 3-node CP runs one prep at a time (never more than one member short of
// quorum), 5-node runs two; a floor of 1 keeps prep progressing.
func TestCPMaxConcurrent(t *testing.T) {
	cases := map[int]int{1: 1, 2: 1, 3: 1, 5: 2, 7: 3, 0: 1}
	for in, want := range cases {
		if got := CPMaxConcurrent(in); got != want {
			t.Errorf("CPMaxConcurrent(%d) = %d, want %d", in, got, want)
		}
	}
}

// BusyControlPlane: a member consumes the window once admitted or once past
// Provisioning; a queued peer (Provisioning + MaintenanceAdmitted=False)
// must NOT count, or every member waits on every other and the window
// deadlocks (found live on the 3-node CP: all three held with "2 other
// control-plane node(s) mid-prep").
func TestBusyControlPlane(t *testing.T) {
	cases := []struct {
		phase    v1alpha1.Phase
		admitted string
		wantBusy bool
	}{
		{v1alpha1.PhasePending, "False", false},
		{v1alpha1.PhaseProvisioning, "False", false}, // waiting, not busy
		{v1alpha1.PhaseProvisioning, "True", true},   // admitted → busy
		{v1alpha1.PhaseFlashing, "False", true},      // past Provisioning → busy
		{v1alpha1.PhaseConfiguring, "", true},
		{v1alpha1.PhaseFinalizing, "", true},
		{v1alpha1.PhaseReady, "", false},  // converged → window released
		{v1alpha1.PhaseFailed, "", false}, // failed → window released
		{"", "", false},
	}
	for _, tc := range cases {
		if got := BusyControlPlane(tc.phase, tc.admitted); got != tc.wantBusy {
			t.Errorf("BusyControlPlane(%v, %q) = %v, want %v", tc.phase, tc.admitted, got, tc.wantBusy)
		}
	}
}

// ExcludeLabelMatch: bare key matches any value, key=value exact, empty
// excludes nothing.
func TestExcludeLabelMatch(t *testing.T) {
	labels := map[string]string{"nodeprep/exclude": "true", "team": "infra"}
	if !ExcludeLabelMatch("nodeprep/exclude", labels) {
		t.Error("bare key must match any value")
	}
	if !ExcludeLabelMatch("nodeprep/exclude=true", labels) {
		t.Error("exact key=value must match")
	}
	if ExcludeLabelMatch("nodeprep/exclude=false", labels) {
		t.Error("wrong value must not match")
	}
	if !ExcludeLabelMatch("team", labels) {
		t.Error("bare key must match the present team label too")
	}
	if ExcludeLabelMatch("other/key", labels) {
		t.Error("an absent key must not match")
	}
	if ExcludeLabelMatch("", labels) {
		t.Error("empty excludeLabel excludes nothing")
	}
	if ExcludeLabelMatch("nodeprep/exclude", map[string]string{}) {
		t.Error("node without the label must not match")
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

// matchesSelection covers the three modes plus the exclusion escape hatch.
func TestMatchesSelection(t *testing.T) {
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "w1",
		Labels: map[string]string{"node.spectrocloud.com/ai-worker": "true"},
	}}
	cp := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "cp1",
		Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
	}}
	ls := func(k, v string) *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchLabels: map[string]string{k: v}}
	}

	// Legacy top-level nodeSelector still gates when selection is absent.
	legacy := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{NodeSelector: ls("node.spectrocloud.com/ai-worker", "true")}}
	if !matchesSelection(legacy, worker) || matchesSelection(legacy, cp) {
		t.Error("legacy nodeSelector must keep working")
	}

	// labelSelector inside selection.
	lab := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		Selection: &v1alpha1.SelectionSpec{Mode: "labelSelector", NodeSelector: ls("node.spectrocloud.com/ai-worker", "true")},
	}}
	if !matchesSelection(lab, worker) || matchesSelection(lab, cp) {
		t.Error("selection.mode=labelSelector must gate on selection.nodeSelector")
	}

	// allWorkers: every node without a CP role.
	allW := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		Selection: &v1alpha1.SelectionSpec{Mode: "allWorkers"},
	}}
	if !matchesSelection(allW, worker) {
		t.Error("allWorkers must adopt an unlabeled worker")
	}
	if matchesSelection(allW, cp) {
		t.Error("allWorkers must not adopt a control plane")
	}

	// allNodes: workers and control planes.
	allN := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		Selection: &v1alpha1.SelectionSpec{Mode: "allNodes"},
	}}
	if !matchesSelection(allN, worker) || !matchesSelection(allN, cp) {
		t.Error("allNodes must adopt both workers and control planes")
	}

	// ExcludeLabel disqualifies under every mode.
	excl := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		Selection: &v1alpha1.SelectionSpec{Mode: "allNodes", ExcludeLabel: "nodeprep-test/skip=true"},
	}}
	skip := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "cp2",
		Labels: map[string]string{"nodeprep-test/skip": "true"},
	}}
	if matchesSelection(excl, skip) {
		t.Error("an excluded node must never be adopted")
	}
	exclAny := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		Selection: &v1alpha1.SelectionSpec{Mode: "allNodes", ExcludeLabel: "nodeprep-test/skip"},
	}}
	if matchesSelection(exclAny, skip) {
		t.Error("bare-key exclusion must match any value")
	}

	// No selector anywhere: no match.
	empty := &v1alpha1.NodePrepProfile{}
	if matchesSelection(empty, worker) {
		t.Error("a profile with no selection must adopt nothing")
	}
}
