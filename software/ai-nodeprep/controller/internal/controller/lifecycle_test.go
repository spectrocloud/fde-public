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
	// The node and the NodePrep agree on the running boot: the harness tests
	// the label/legacy logic on a verified CURRENT boot, not the boot gate
	// (TestLifecycleWorkerLabelBootGate owns the mismatch cells).
	node.Status.NodeInfo.BootID = "boot-1"
	client := clientfake.NewSimpleClientset(node)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodePrepsGVR: "NodePrepList"})
	c := New(client, dyn, "nodeprep-system")
	pol.WorkerRoleLabel = "ignore" // keep the legacy label the only label under test
	profile := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{Policy: pol}}
	np := &v1alpha1.NodePrep{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	np.Status.Phase = v1alpha1.PhaseReady
	np.Status.BootID = "boot-1"
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

// bootLabelRun drives one lifecycle pass with explicit boot identities and
// walk phase — the cells the boot gate (0.1.94) turns on.
func bootLabelRun(t *testing.T, nodeBootID, npBootID string, phase v1alpha1.Phase, bootVerified bool, pol v1alpha1.PolicySpec) (*clientfake.Clientset, *corev1.Node) {
	t.Helper()
	labels := map[string]string{v1alpha1.WorkerRoleLabel: ""} // label present pre-walk
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: labels}}
	if nodeBootID != "" {
		node.Status.NodeInfo.BootID = nodeBootID
	}
	client := clientfake.NewSimpleClientset(node)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodePrepsGVR: "NodePrepList"})
	c := New(client, dyn, "nodeprep-system")
	np := &v1alpha1.NodePrep{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	np.Status.Phase = phase
	np.Status.BootID = npBootID
	bv := metav1.ConditionFalse
	if bootVerified {
		bv = metav1.ConditionTrue
	}
	conds := []metav1.Condition{{
		Type:               v1alpha1.ConditionBootVerified,
		Status:             bv,
		Reason:             v1alpha1.ReasonVerified,
		LastTransitionTime: metav1.Now(),
	}}
	if phase == v1alpha1.PhaseReady && bootVerified {
		conds = append(conds, metav1.Condition{
			Type:               v1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ReasonVerified,
			Message:            "node prepared and boot verified",
			LastTransitionTime: metav1.Now(),
		})
	}
	np.Status.Conditions = conds
	profile := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{Policy: pol}}
	c.lifecycle(context.Background(), node, profile, np, false)
	return client, node
}

func getNode(t *testing.T, client *clientfake.Clientset) *corev1.Node {
	t.Helper()
	n, err := client.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	return n
}

func nodeHasTaint(t *testing.T, node *corev1.Node) bool {
	t.Helper()
	for _, ta := range node.Spec.Taints {
		if ta.Key == v1alpha1.TaintKey && ta.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

// The 0.1.94 boot gate wired through the full lifecycle: the worker label
// follows the CURRENT boot, and the taint front-runs it — the SR-IOV Network
// Config Daemon's DaemonSet only schedules onto labelled nodes, so the
// label strip must land at the node's first post-boot status post (a node
// event), not at the agent's boot-detect patch.
func TestLifecycleWorkerLabelBootGate(t *testing.T) {
	pol := v1alpha1.PolicySpec{} // taints + worker label managed

	// Verified boot, phase Ready, bootIDs agree: label stays, taint stays off.
	client, _ := bootLabelRun(t, "boot-1", "boot-1", v1alpha1.PhaseReady, true, pol)
	node := getNode(t, client)
	if _, ok := node.Labels[v1alpha1.WorkerRoleLabel]; !ok {
		t.Fatalf("a verified current boot must keep the worker label, got %v", node.Labels)
	}
	if nodeHasTaint(t, node) {
		t.Fatal("a verified current boot must not carry the taint")
	}

	// The node rebooted (mismatch) while the status still reads Ready +
	// verified: label stripped AND taint front-run in the same pass.
	client, _ = bootLabelRun(t, "boot-2", "boot-1", v1alpha1.PhaseReady, true, pol)
	node = getNode(t, client)
	if _, ok := node.Labels[v1alpha1.WorkerRoleLabel]; ok {
		t.Fatalf("a new unverified boot must strip the worker label, got %v", node.Labels)
	}
	if !nodeHasTaint(t, node) {
		t.Fatal("a new unverified boot must carry the taint (front-run)")
	}

	// Mid-walk: label stripped and taint held even with everything agreeing.
	client, _ = bootLabelRun(t, "boot-1", "boot-1", v1alpha1.PhaseConfiguring, false, pol)
	node = getNode(t, client)
	if _, ok := node.Labels[v1alpha1.WorkerRoleLabel]; ok {
		t.Fatalf("a mid-walk phase must strip the worker label, got %v", node.Labels)
	}
	if !nodeHasTaint(t, node) {
		t.Fatal("a mid-walk phase must hold the taint")
	}

	// workerRoleLabel=ignore: the label is never touched (the pre-existing
	// label survives the Ready+verified+matching pass AND the mismatch
	// pass); the taint contract is independent of it.
	ignore := v1alpha1.PolicySpec{WorkerRoleLabel: "ignore"}
	client, _ = bootLabelRun(t, "boot-1", "boot-1", v1alpha1.PhaseReady, true, ignore)
	node = getNode(t, client)
	if _, ok := node.Labels[v1alpha1.WorkerRoleLabel]; !ok {
		t.Fatal("ignore must leave the worker label alone")
	}
	client, _ = bootLabelRun(t, "boot-2", "boot-1", v1alpha1.PhaseReady, true, ignore)
	node = getNode(t, client)
	if _, ok := node.Labels[v1alpha1.WorkerRoleLabel]; !ok {
		t.Fatal("ignore must leave the worker label alone on a boot mismatch too")
	}
	if !nodeHasTaint(t, node) {
		t.Fatal("the taint front-run is independent of workerRoleLabel")
	}
}
