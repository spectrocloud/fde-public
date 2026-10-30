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
