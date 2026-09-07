package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientfake "k8s.io/client-go/kubernetes/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"spectrocloud.com/nodeprep/api/v1alpha1"
	"spectrocloud.com/nodeprep/internal/k8sutil"
)

// The reboot checkpoint must gate the walk, not ride it: a recorded request
// blocks the stage transition (bash v105 L938-943, NEEDREBOOT re-enters the
// stage), a halt-now request stops the pass itself (bash L353, DOCA
// "rebooting now" mid-stage), and the first quiet pass fires whatever
// accumulated. Found live on DSX Air 0.1.65: both requests rode the quiet
// checkpoint while the walk kept advancing — mlxconfig ran in the same pass
// DOCA finished, and the Finalizing steps then ran against the
// staged-but-unapplied firmware state.

// newRebootGateAgent seeds a dynamic fake with np (so patchCondition and
// fetchNodePrep round-trip) and a worker-role node (so the Provisioning
// admission gates pass). Reboots are not allowed: requestReboot then only
// records the condition instead of arming the 60s exec goroutine.
func newRebootGateAgent(t *testing.T, np *v1alpha1.NodePrep) *Agent {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodePrepsGVR: "NodePrepList"},
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "nodeprep.spectrocloud.com/v1alpha1",
			"kind":       "NodePrep",
			"metadata":   map[string]interface{}{"name": np.Name},
			"status":     map[string]interface{}{"phase": string(np.Status.Phase)},
		}},
	)
	return &Agent{
		nodeName: np.Name,
		dyn:      dyn,
		client:   clientfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: np.Name}}),
	}
}

// A deferred request must not stop the pass (the stage's remaining steps
// still run — the bash batches NEEDREBOOT across the stage), but the walk
// must not advance to the next stage over it; the first quiet pass fires
// the request instead.
func TestRunStageAdvanceBlocksOnPendingReboot(t *testing.T) {
	orig := stepDefs
	defer func() { stepDefs = orig }()
	laterRan := false
	stepDefs = []stepDef{
		{name: "fakeRebooter", stage: v1alpha1.PhaseConfiguring,
			run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
				a.requestRebootBg(v1alpha1.RebootGrubChanged, "fake grub change", "")
				return v1alpha1.StepDone, "fake grub written"
			}},
		{name: "fakeLater", stage: v1alpha1.PhaseConfiguring,
			run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
				laterRan = true
				return v1alpha1.StepDone, "fake later converged"
			}},
	}
	np := &v1alpha1.NodePrep{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	np.Status.Phase = v1alpha1.PhaseConfiguring
	a := newRebootGateAgent(t, np)
	ctx := context.Background()

	a.runStage(ctx, np, &v1alpha1.NodePrepProfile{})
	if !laterRan {
		t.Fatal("a deferred request must not stop the pass; the stage's remaining steps still run")
	}
	if len(a.pendingReboot) != 1 {
		t.Fatalf("the request must stay recorded, got %+v", a.pendingReboot)
	}
	if np.Status.Phase != v1alpha1.PhaseConfiguring {
		t.Fatalf("the walk advanced to %s with a reboot request pending", np.Status.Phase)
	}

	// Pass 2 is quiet (both steps Done), so the checkpoint fires the
	// request; the stage transition stays blocked until the boot clears it.
	a.runStage(ctx, np, &v1alpha1.NodePrepProfile{})
	a.checkpointReboot(ctx)
	if len(a.pendingReboot) != 0 {
		t.Fatalf("the quiet pass must fire the request, got %+v", a.pendingReboot)
	}
	if np.Status.Phase != v1alpha1.PhaseConfiguring {
		t.Fatalf("the walk advanced to %s before the reboot landed", np.Status.Phase)
	}
	fresh, err := a.fetchNodePrep(ctx)
	if err != nil {
		t.Fatalf("fetch after fire: %v", err)
	}
	if k8sutil.ConditionStatus(fresh.Status.Conditions, v1alpha1.ConditionRebootRequired) != "True" {
		t.Fatalf("RebootRequired must be True after the checkpoint fired, got %+v", fresh.Status.Conditions)
	}
}

// A halt-now request stops the pass the moment it is recorded: later steps
// must not run against a host that is about to reboot (bash L353 reboots
// mid-stage), the walk must not advance over the steps that never ran, and
// the hold pass is the quiet pass the checkpoint fires from.
func TestRunStageHaltNowStopsPass(t *testing.T) {
	orig := stepDefs
	defer func() { stepDefs = orig }()
	laterRan := false
	stepDefs = []stepDef{
		{name: "fakeDoca", stage: v1alpha1.PhaseProvisioning,
			run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
				a.requestRebootHalt(v1alpha1.RebootDocaInstalled, "fake doca installed", "")
				return v1alpha1.StepDone, "fake doca installed"
			}},
		{name: "fakeLater", stage: v1alpha1.PhaseProvisioning,
			run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
				laterRan = true
				return v1alpha1.StepDone, "fake later converged"
			}},
	}
	np := &v1alpha1.NodePrep{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	np.Status.Phase = v1alpha1.PhaseProvisioning
	a := newRebootGateAgent(t, np)
	ctx := context.Background()

	a.runStage(ctx, np, &v1alpha1.NodePrepProfile{})
	if laterRan {
		t.Fatal("a halt-now request must stop the pass; later steps must not run pre-reboot")
	}
	if len(a.pendingReboot) != 1 || !a.pendingReboot[0].haltNow {
		t.Fatalf("one halt-now request expected, got %+v", a.pendingReboot)
	}
	if np.Status.Phase != v1alpha1.PhaseProvisioning {
		t.Fatalf("the walk advanced to %s over steps that never ran", np.Status.Phase)
	}

	// Pass 2 holds before the step loop and is the quiet pass that fires.
	a.runStage(ctx, np, &v1alpha1.NodePrepProfile{})
	if laterRan {
		t.Fatal("the halt hold pass must not run steps either")
	}
	a.checkpointReboot(ctx)
	if len(a.pendingReboot) != 0 {
		t.Fatalf("the halt hold pass is quiet and must fire the request, got %+v", a.pendingReboot)
	}
}

func TestRequestRebootHaltFlag(t *testing.T) {
	a := &Agent{client: clientfake.NewSimpleClientset()}
	a.requestRebootBg(v1alpha1.RebootGrubChanged, "deferred", "")
	if a.hasHaltNowRequest() {
		t.Fatal("a deferred request must not halt the pass")
	}
	a.requestRebootHalt(v1alpha1.RebootDocaInstalled, "halt", "")
	if !a.hasHaltNowRequest() {
		t.Fatal("the halt-now request must halt the pass")
	}
	if len(a.pendingReboot) != 2 {
		t.Fatalf("two distinct requests expected, got %+v", a.pendingReboot)
	}
	// Dedup: re-recording either reason changes nothing.
	a.requestRebootHalt(v1alpha1.RebootDocaInstalled, "halt again", "")
	a.requestRebootBg(v1alpha1.RebootGrubChanged, "deferred again", "")
	if len(a.pendingReboot) != 2 {
		t.Fatalf("requests must dedup by reason, got %+v", a.pendingReboot)
	}
}
