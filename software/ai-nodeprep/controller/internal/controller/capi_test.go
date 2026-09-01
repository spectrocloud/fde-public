package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgotesting "k8s.io/client-go/testing"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// machineFor builds a fake CAPI Machine whose nodeRef points at nodeName,
// with optional starting annotations.
func machineFor(nodeName string, annotations map[string]interface{}) *unstructured.Unstructured {
	if annotations == nil {
		annotations = map[string]interface{}{}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cluster.x-k8s.io/v1beta1",
		"kind":       "Machine",
		"metadata": map[string]interface{}{
			"name":        "worker-1",
			"namespace":   "infra",
			"annotations": annotations,
		},
		"status": map[string]interface{}{"nodeRef": map[string]interface{}{"name": nodeName}},
	}}
}

func fakeMachineClient(m *unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{machineGVR: "MachineList"},
		m)
}

// The pause must be observable: every actual state change returns a message
// (surfaced as an event by lifecycle), a no-op returns none, and a failed
// patch is an error — never silence.
func TestReconcileCAPAPauseTransitions(t *testing.T) {
	ctx := context.Background()

	c := fakeMachineClient(machineFor("node-1", nil))
	if msg, err := reconcileCAPAPause(ctx, c, "node-1", v1alpha1.PhaseFlashing, true); err != nil || msg == "" {
		t.Fatalf("pause of an unpaused machine: msg=%q err=%v, want a transition message", msg, err)
	}
	got, _ := c.Resource(machineGVR).Namespace("infra").Get(ctx, "worker-1", metav1.GetOptions{})
	if _, ok := got.GetAnnotations()[v1alpha1.CAPAPauseAnnotation]; !ok {
		t.Fatalf("pause annotation missing after pause: %v", got.GetAnnotations())
	}

	if msg, err := reconcileCAPAPause(ctx, c, "node-1", v1alpha1.PhaseFlashing, true); err != nil || msg != "" {
		t.Fatalf("second pause must be a no-op: msg=%q err=%v", msg, err)
	}

	if msg, err := reconcileCAPAPause(ctx, c, "node-1", v1alpha1.PhaseReady, false); err != nil || msg == "" {
		t.Fatalf("unpause at Ready: msg=%q err=%v, want a transition message", msg, err)
	}
	got, _ = c.Resource(machineGVR).Namespace("infra").Get(ctx, "worker-1", metav1.GetOptions{})
	if _, ok := got.GetAnnotations()[v1alpha1.CAPAPauseAnnotation]; ok {
		t.Fatalf("pause annotation still present after unpause: %v", got.GetAnnotations())
	}

	if msg, err := reconcileCAPAPause(ctx, c, "node-1", v1alpha1.PhaseReady, false); err != nil || msg != "" {
		t.Fatalf("second unpause must be a no-op: msg=%q err=%v", msg, err)
	}

	for _, nodeName := range []string{"node-other", ""} {
		if msg, err := reconcileCAPAPause(ctx, c, nodeName, v1alpha1.PhaseFlashing, true); err != nil || msg != "" {
			t.Fatalf("node %q with no matching Machine: msg=%q err=%v", nodeName, msg, err)
		}
	}
}

// A failed pause patch must come back as an error, not disappear: the
// operator has to hear that the Machine is NOT actually paused.
func TestReconcileCAPAPauseFailureSurfaces(t *testing.T) {
	ctx := context.Background()
	c := fakeMachineClient(machineFor("node-1", nil))
	c.PrependReactor("patch", "machines", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	msg, err := reconcileCAPAPause(ctx, c, "node-1", v1alpha1.PhaseFlashing, true)
	if err == nil || !strings.Contains(err.Error(), "pausing Machine infra/worker-1") {
		t.Fatalf("failed pause must surface as an error naming the machine, got msg=%q err=%v", msg, err)
	}
	if msg != "" {
		t.Fatalf("a failed pause must not claim a transition: %q", msg)
	}
}

// De-adoption releases the Machine: a stale pause with no NodePrep left
// would block CAPI from reconciling the Machine forever.
func TestClearMachinePauseOnDeadoption(t *testing.T) {
	ctx := context.Background()
	ann := map[string]interface{}{v1alpha1.CAPAPauseAnnotation: ""}
	c := fakeMachineClient(machineFor("node-1", ann))
	m, _ := c.Resource(machineGVR).Namespace("infra").Get(ctx, "worker-1", metav1.GetOptions{})
	changed, err := clearMachinePause(ctx, c, m)
	if err != nil || !changed {
		t.Fatalf("clearing a present annotation: changed=%v err=%v", changed, err)
	}
	m, _ = c.Resource(machineGVR).Namespace("infra").Get(ctx, "worker-1", metav1.GetOptions{})
	if _, ok := m.GetAnnotations()[v1alpha1.CAPAPauseAnnotation]; ok {
		t.Fatalf("annotation survived the clear")
	}
}
