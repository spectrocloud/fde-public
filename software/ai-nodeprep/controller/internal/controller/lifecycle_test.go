package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfake "k8s.io/client-go/kubernetes/fake"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// lifecycleHarness builds a Controller over fake clients with a Ready,
// boot-verified worker node — the shape that exercises only the label
// logic (no taint transitions, no admission windows, no CAPI pause).
func lifecycleHarness(t *testing.T, nodeLabels map[string]string, pol v1alpha1.PolicySpec) (*clientfake.Clientset, *corev1.Node) {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: nodeLabels}}
	client := clientfake.NewSimpleClientset(node)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodePrepsGVR: "NodePrepList"})
	c := New(client, dyn, "nodeprep-system")
	pol.WorkerRoleLabel = "ignore" // keep the legacy label the only label under test
	profile := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{Policy: pol}}
	np := &v1alpha1.NodePrep{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	np.Status.Phase = v1alpha1.PhaseReady
	// Both conditions pre-baked exactly as readyFor/TaintShouldExist compute
	// them, so the pass is a pure no-op for conditions (no dyn writes).
	np.Status.Conditions = []metav1.Condition{
		{
			Type:               v1alpha1.ConditionBootVerified,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ReasonVerified,
			LastTransitionTime: metav1.Now(),
		},
		{
			Type:               v1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ReasonVerified,
			Message:            "node prepared and boot verified",
			LastTransitionTime: metav1.Now(),
		},
	}
	c.lifecycle(context.Background(), node, profile, np, false)
	return client, node
}

// nodeUpdates counts node write actions the lifecycle issued.
func nodeUpdates(t *testing.T, client *clientfake.Clientset) int {
	t.Helper()
	n := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "nodes" {
			n++
		}
	}
	return n
}

// The legacy state label is the bash-migration shim (policy.labelCompat).
// The default (false) never writes it and REMOVES it from any node still
// carrying a bash-era or v1-mirrored value — nothing outside the controller
// reads the label anymore. labelCompat: true restores the v1-era phase
// mirror (phases.LegacyFor). The delete is a no-op on a clean node, so the
// 30s resync never churns it.
func TestLifecycleLegacyLabel(t *testing.T) {
	ctx := context.Background()

	// Default profile: the label is removed from the node, in one update.
	client, _ := lifecycleHarness(t, map[string]string{v1alpha1.LegacyLabel: "complete"}, v1alpha1.PolicySpec{})
	if n := nodeUpdates(t, client); n != 1 {
		t.Fatalf("label removal must issue exactly one node update, got %d", n)
	}
	got, err := client.CoreV1().Nodes().Get(ctx, "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if _, ok := got.Labels[v1alpha1.LegacyLabel]; ok {
		t.Fatalf("labelCompat default (false) must remove the legacy label, got %v", got.Labels)
	}

	// Idempotence: a node without the label must not be rewritten — the
	// 30s informer resync calls lifecycle again, and only a real diff may
	// touch the node.
	client2, _ := lifecycleHarness(t, map[string]string{}, v1alpha1.PolicySpec{})
	if n := nodeUpdates(t, client2); n != 0 {
		t.Fatalf("a node without the label must not be updated (resync churn), got %d updates", n)
	}

	// labelCompat: true restores the mirror: the phase maps onto the bash
	// label value (Ready → complete).
	client3, _ := lifecycleHarness(t, map[string]string{}, v1alpha1.PolicySpec{LabelCompat: true})
	if n := nodeUpdates(t, client3); n != 1 {
		t.Fatalf("the mirror must write the label once, got %d updates", n)
	}
	got3, _ := client3.CoreV1().Nodes().Get(ctx, "node-1", metav1.GetOptions{})
	if v := got3.Labels[v1alpha1.LegacyLabel]; v != "complete" {
		t.Fatalf("labelCompat: true must mirror the phase (Ready → complete), got %q in %v", v, got3.Labels)
	}
}
