package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A freshly registered control plane carries its taint before kubeadm adds
// the role label; both signals must classify it (1.1.0, vm-cp-pool-03).
func TestIsControlPlane(t *testing.T) {
	cases := []struct {
		name string
		node *corev1.Node
		want bool
	}{
		{"label", &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"node-role.kubernetes.io/control-plane": ""}}}, true},
		{"master label", &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"node-role.kubernetes.io/master": ""}}}, true},
		{"taint only", &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}}}, true},
		{"master taint only", &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: "node-role.kubernetes.io/master", Effect: corev1.TaintEffectNoSchedule}}}}, true},
		{"worker", &corev1.Node{}, false},
		{"worker with unrelated taint", &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}}}}, false},
	}
	for _, c := range cases {
		if got := isControlPlane(c.node); got != c.want {
			t.Errorf("%s: isControlPlane = %v, want %v", c.name, got, c.want)
		}
	}
}

// Adoption waits for the node's first Ready status so the control-plane
// role signals are in place; a registration-time adoption used to race
// them (vm-cp-pool-02, 1.1.0).
func TestAdoptionWaitsForReady(t *testing.T) {
	cases := []struct {
		name string
		cond *corev1.NodeCondition
		want bool
	}{
		{"no condition", nil, false},
		{"unknown", &corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}, false},
		{"false", &corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse}, false},
		{"true", &corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}, true},
	}
	for _, c := range cases {
		node := &corev1.Node{}
		// Decoys must not satisfy the check.
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}}
		if c.cond != nil {
			node.Status.Conditions = append(node.Status.Conditions, *c.cond)
		}
		if got := nodeReady(node); got != c.want {
			t.Errorf("%s: nodeReady = %v, want %v", c.name, got, c.want)
		}
	}
}
