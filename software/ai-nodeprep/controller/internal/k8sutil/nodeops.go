package k8sutil

import (
	corev1 "k8s.io/api/core/v1"
)

// Pure decision + mutation helpers over Node objects. The controller applies
// taints, the worker-role label and the legacy mirror label through
// ApplyNodeChanges; the decision functions are unit-tested.

// HasTaint reports whether the node carries key with NoSchedule effect.
func HasTaint(node *corev1.Node, key string) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == key && t.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

// WithTaint returns a copy of taints with key added (idempotent).
func WithTaint(taints []corev1.Taint, key string) []corev1.Taint {
	out := make([]corev1.Taint, len(taints), len(taints)+1)
	copy(out, taints)
	for _, t := range out {
		if t.Key == key && t.Effect == corev1.TaintEffectNoSchedule {
			return out
		}
	}
	return append(out, corev1.Taint{Key: key, Effect: corev1.TaintEffectNoSchedule})
}

// WithoutTaint returns a copy of taints with key removed.
func WithoutTaint(taints []corev1.Taint, key string) []corev1.Taint {
	out := taints[:0:0]
	for _, t := range taints {
		if !(t.Key == key && t.Effect == corev1.TaintEffectNoSchedule) {
			out = append(out, t)
		}
	}
	return out
}

// ApplyNodeChanges mutates node per the desired state and reports whether
// anything changed. desired map values: label value, or "" to set empty,
// or "\x00delete" to remove the label.
func ApplyNodeChanges(node *corev1.Node, wantTaint bool, taintKey string, labels map[string]string) bool {
	changed := false
	if wantTaint && !HasTaint(node, taintKey) {
		node.Spec.Taints = WithTaint(node.Spec.Taints, taintKey)
		changed = true
	}
	if !wantTaint && HasTaint(node, taintKey) {
		node.Spec.Taints = WithoutTaint(node.Spec.Taints, taintKey)
		changed = true
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	for k, v := range labels {
		cur, exists := node.Labels[k]
		switch {
		case v == "\x00delete":
			if exists {
				delete(node.Labels, k)
				changed = true
			}
		default:
			if !exists || cur != v {
				node.Labels[k] = v
				changed = true
			}
		}
	}
	return changed
}
