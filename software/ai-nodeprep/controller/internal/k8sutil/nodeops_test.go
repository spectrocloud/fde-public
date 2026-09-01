package k8sutil

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func nodeWith(taints []corev1.Taint, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: labels}, Spec: corev1.NodeSpec{Taints: taints}}
}

func TestTaintLifecycle(t *testing.T) {
	key := "spectrocloud.com/nodeprep"
	n := nodeWith(nil, nil)
	if HasTaint(n, key) {
		t.Fatal("fresh node should not have taint")
	}
	n.Spec.Taints = WithTaint(n.Spec.Taints, key)
	if !HasTaint(n, key) {
		t.Fatal("taint should be present after WithTaint")
	}
	n.Spec.Taints = WithTaint(n.Spec.Taints, key) // idempotent
	if len(n.Spec.Taints) != 1 {
		t.Fatalf("WithTaint must be idempotent, got %d", len(n.Spec.Taints))
	}
	n.Spec.Taints = WithoutTaint(n.Spec.Taints, key)
	if HasTaint(n, key) || len(n.Spec.Taints) != 0 {
		t.Fatal("taint should be gone after WithoutTaint")
	}
}

func TestApplyNodeChanges(t *testing.T) {
	key := "spectrocloud.com/nodeprep"
	n := nodeWith(nil, map[string]string{"spectrocloud.com/nodeprep": "complete"})

	if ApplyNodeChanges(n, false, key, nil) {
		t.Error("no-op apply must report unchanged")
	}
	if !ApplyNodeChanges(n, true, key, nil) {
		t.Error("taint add must report changed")
	}
	if ApplyNodeChanges(n, true, key, nil) {
		t.Error("idempotent taint add must report unchanged")
	}
	if !ApplyNodeChanges(n, true, key, map[string]string{"x": "1"}) {
		t.Error("label add must report changed")
	}
	if !ApplyNodeChanges(n, false, key, map[string]string{"x": "\x00delete"}) {
		t.Error("taint removal + label delete must report changed")
	}
	if HasTaint(n, key) {
		t.Error("taint should have been removed")
	}
	if _, ok := n.Labels["x"]; ok {
		t.Error("label should have been deleted")
	}
}

func TestSetCondition(t *testing.T) {
	var conds []metav1.Condition
	if !SetCondition(&conds, "Ready", "False", "Converging", "working", 1) {
		t.Fatal("first set must report change")
	}
	if SetCondition(&conds, "Ready", "False", "Converging", "working", 1) {
		t.Error("identical set must not report change")
	}
	if !SetCondition(&conds, "Ready", "True", "Verified", "done", 1) {
		t.Error("status flip must report change")
	}
	if ConditionStatus(conds, "Ready") != "True" {
		t.Error("ConditionStatus should read True")
	}
	if ConditionStatus(conds, "Absent") != "" {
		t.Error("missing condition should read empty")
	}
}
