package agent

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// bootVerify re-runs the critical step bodies on a Ready node. Before
// 0.1.48 it patched only the ledger messages, so the boot-transient
// re-open (a boot change landing on an already-Ready node) left
// sriovNumVFs/vfGuids/udevRules/disableACS Pending in the ledger forever
// even though the verify pass had just run their bodies to Done — found
// live on bl-r1-c2-02 boot #7: phase Ready, BootVerified True, taint
// released, and four steps showing Pending against a converged host.
func TestBootVerifyWritesStepStates(t *testing.T) {
	orig := stepDefs
	defer func() { stepDefs = orig }()
	stepDefs = []stepDef{
		{name: "fakeCritical", stage: v1alpha1.PhaseFinalizing, critical: true,
			run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
				return v1alpha1.StepDone, "fake critical converged"
			}},
		{name: "fakeSoft", stage: v1alpha1.PhaseFinalizing, critical: false,
			run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
				t.Fatal("non-critical steps must not run in boot-verify")
				return v1alpha1.StepDone, ""
			}},
	}

	np := &v1alpha1.NodePrep{Status: v1alpha1.NodePrepStatus{
		Steps: []v1alpha1.StepStatus{
			{Name: "fakeCritical", Stage: v1alpha1.PhaseFinalizing, State: v1alpha1.StepPending, Message: "stale provisioning-era message"},
			{Name: "fakeSoft", Stage: v1alpha1.PhaseFinalizing, State: v1alpha1.StepPending, Message: "stale"},
		},
	}}
	if !newVerifyTestAgent(t).bootVerify(context.Background(), np, &v1alpha1.NodePrepProfile{}) {
		t.Fatal("bootVerify must pass: the fake critical step returns Done")
	}

	got := np.Status.Steps
	if got[0].State != v1alpha1.StepDone || got[0].Message != "fake critical converged" {
		t.Fatalf("critical step ledger not updated: state=%s msg=%q", got[0].State, got[0].Message)
	}
	if got[1].State != v1alpha1.StepPending || got[1].Message != "stale" {
		t.Fatalf("non-critical step must be untouched: state=%s msg=%q", got[1].State, got[1].Message)
	}
}

// A non-Done verify result is written to the ledger as well — it is the
// step's latest outcome — and fails the pass (BootVerified flips False and
// the taint is re-applied upstream in verifyReady). The ledger must not keep
// claiming Done with a fresher failure message pasted next to it.
func TestBootVerifyWritesNonDoneResult(t *testing.T) {
	orig := stepDefs
	defer func() { stepDefs = orig }()
	stepDefs = []stepDef{
		{name: "fakeCritical", stage: v1alpha1.PhaseFinalizing, critical: true,
			run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
				return v1alpha1.StepBlocked, "fake drift"
			}},
	}

	np := &v1alpha1.NodePrep{Status: v1alpha1.NodePrepStatus{
		Steps: []v1alpha1.StepStatus{
			{Name: "fakeCritical", Stage: v1alpha1.PhaseFinalizing, State: v1alpha1.StepDone, Message: "provisioned cleanly"},
		},
	}}
	if newVerifyTestAgent(t).bootVerify(context.Background(), np, &v1alpha1.NodePrepProfile{}) {
		t.Fatal("bootVerify must fail when a critical step reports Blocked")
	}

	got := np.Status.Steps[0]
	if got.State != v1alpha1.StepBlocked || got.Message != "fake drift" {
		t.Fatalf("non-Done result not written to the ledger: state=%s msg=%q", got.State, got.Message)
	}
}

// newVerifyTestAgent builds the minimum Agent bootVerify touches: logf
// needs only the receiver and patchStatus a dynamic client (the fake has no
// NodePrep object, so the tolerated status patch fails and is logged).
func newVerifyTestAgent(t *testing.T) *Agent {
	t.Helper()
	return &Agent{
		nodeName: "node-1",
		dyn: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(),
			map[schema.GroupVersionResource]string{nodePrepsGVR: "NodePrepList"},
		),
	}
}
