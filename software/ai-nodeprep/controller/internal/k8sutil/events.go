// Package k8sutil holds small shared Kubernetes helpers: event emission and
// node patching with pure decision functions that unit tests cover.
package k8sutil

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Emit records an Event in ns with the given involved object. Events are the
// human-visible progress channel required by design §2 (observable by default).
// Failures are logged, never fatal.
func Emit(ctx context.Context, c kubernetes.Interface, ns, involvedKind, involvedName, eventType, reason, message string) {
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "nodeprep-",
			Namespace:    ns,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       involvedKind,
			Name:       involvedName,
			APIVersion: "nodeprep.spectrocloud.com/v1alpha1",
		},
		Reason:              reason,
		Message:             message,
		Type:                eventType,
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
		ReportingController: "nodeprep",
		ReportingInstance:   "nodeprep",
		EventTime:           metav1.NewMicroTime(now.Time),
		Source:              corev1.EventSource{Component: "nodeprep"},
	}
	_, err := c.CoreV1().Events(ns).Create(ctx, ev, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("[nodeprep] event %s/%s (%s) not recorded: %v\n", involvedKind, involvedName, reason, err)
	}
}

// SetCondition updates conditions in place with a new or refreshed condition.
// It returns true when the condition changed (callers emit events on change).
func SetCondition(conds *[]metav1.Condition, condType, status, reason, message string, observedGeneration int64) bool {
	now := metav1.NewTime(time.Now().UTC())
	for i := range *conds {
		c := &(*conds)[i]
		if c.Type != condType {
			continue
		}
		if string(c.Status) == status && c.Reason == reason && c.Message == message {
			c.ObservedGeneration = observedGeneration
			return false
		}
		c.Status = metav1.ConditionStatus(status)
		c.Reason = reason
		c.Message = message
		c.ObservedGeneration = observedGeneration
		c.LastTransitionTime = now
		return true
	}
	*conds = append(*conds, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionStatus(status),
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
		LastTransitionTime: now,
	})
	return true
}

// ConditionStatus returns the status string for condType, or "" when absent.
func ConditionStatus(conds []metav1.Condition, condType string) string {
	for _, c := range conds {
		if c.Type == condType {
			return string(c.Status)
		}
	}
	return ""
}
