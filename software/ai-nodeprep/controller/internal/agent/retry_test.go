package agent

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

func TestTransientRetryError(t *testing.T) {
	transient := []string{
		`Get "https://api.example.com:6443/api": dial tcp: lookup api.example.com on 10.0.0.2:53: no such host`,
		`lookup registry.example.com: Temporary failure in name resolution`,
		`Get "https://10.0.0.1:6443/apis": dial tcp 10.0.0.1:6443: connect: connection refused`,
		`read tcp 10.0.0.5:41234->10.0.0.1:6443: read: connection reset by peer`,
		`Get "https://10.0.0.1:6443/api": net/http: TLS handshake timeout`,
		`the server is currently unable to handle the request`,
		`etcdserver: request timed out`,
		`context deadline exceeded`,
		`Client.Timeout exceeded while awaiting headers`,
		`dial tcp 10.0.0.1:6443: i/o timeout`,
		`Too Many Requests`,
	}
	for _, msg := range transient {
		if !transientRetryError(msg) {
			t.Errorf("expected transient: %q", msg)
		}
	}
	permanent := []string{
		`mlxconfig reset failed: device busy`,
		`key LINK_TYPE_P1 absent from query`,
		`unsupported shell expression "$(hostname)"`,
		`sha256 mismatch: got abc, want def`,
		`nf Incorrect XID detected`,
		`apt-get exited 100: package has no installation candidate`,
	}
	for _, msg := range permanent {
		if transientRetryError(msg) {
			t.Errorf("must not be transient: %q", msg)
		}
	}
}

// The first three retries now span about a minute (10s + 20s + 40s), not the
// ~15s the old interval<<(n-1) schedule produced, and the wait is capped.
func TestStepBackoff(t *testing.T) {
	interval := 5 * time.Second
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 2 * time.Minute, 2 * time.Minute}
	for i, w := range want {
		if got := stepBackoff(interval, i+1); got != w {
			t.Errorf("stepBackoff(5s, %d) = %s, want %s", i+1, got, w)
		}
	}
}

// retryTestAgent builds an agent whose stage passes can be driven directly.
func retryTestAgent(t *testing.T, np *v1alpha1.NodePrep) (*Agent, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodePrepsGVR: "NodePrepList"},
	)
	raw := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": v1alpha1.GroupName + "/" + v1alpha1.Version,
		"kind":       "NodePrep",
		"metadata":   map[string]interface{}{"name": "node-1"},
		"spec":       map[string]interface{}{"nodeName": "node-1"},
		"status":     map[string]interface{}{"phase": string(np.Status.Phase)},
	}}
	if err := dyn.Tracker().Add(raw); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		nodeName:          "node-1",
		dyn:               dyn,
		client:            k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}),
		interval:          5 * time.Second,
		backoffUntil:      map[string]time.Time{},
		transientFailures: map[string]int{},
	}
	return a, dyn
}

func currentPhase(t *testing.T, dyn *dynamicfake.FakeDynamicClient) string {
	t.Helper()
	u, err := dyn.Resource(nodePrepsGVR).Get(context.Background(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	return phase
}

// Ten counted failures exhaust the budget; nine do not.
func TestRunStageCountsTenAttempts(t *testing.T) {
	orig := stepDefs
	t.Cleanup(func() { stepDefs = orig })
	stepDefs = []stepDef{{
		name: "alwaysFails", stage: v1alpha1.PhaseConfiguring,
		run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
			return v1alpha1.StepFailed, "mlxconfig reset failed: device busy"
		},
	}}

	np := &v1alpha1.NodePrep{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status:     v1alpha1.NodePrepStatus{Phase: v1alpha1.PhaseConfiguring},
	}
	a, dyn := retryTestAgent(t, np)
	for i := 1; i <= 9; i++ {
		a.backoffUntil = map[string]time.Time{}
		a.runStage(context.Background(), np, &v1alpha1.NodePrepProfile{})
		if p := currentPhase(t, dyn); p == string(v1alpha1.PhaseFailed) {
			t.Fatalf("NodePrep failed after %d attempts; the budget is %d", i, maxStepAttempts)
		}
	}
	a.backoffUntil = map[string]time.Time{}
	a.runStage(context.Background(), np, &v1alpha1.NodePrepProfile{})
	if p := currentPhase(t, dyn); p != string(v1alpha1.PhaseFailed) {
		t.Fatalf("the %dth counted failure must exhaust the budget, phase=%q", maxStepAttempts, p)
	}
}

// Transient infrastructure failures never consume the counted budget, no
// matter how long the outage lasts, and a later success clears the pacing.
func TestRunStageTransientFailuresDoNotCount(t *testing.T) {
	orig := stepDefs
	t.Cleanup(func() { stepDefs = orig })
	fail := true
	stepDefs = []stepDef{{
		name: "waitsForAPI", stage: v1alpha1.PhaseConfiguring,
		run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
			if fail {
				return v1alpha1.StepFailed, `Get "https://api.example.com:6443/api": dial tcp: lookup api.example.com on 10.0.0.2:53: no such host`
			}
			return v1alpha1.StepDone, "apiserver reachable"
		},
	}}

	np := &v1alpha1.NodePrep{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status:     v1alpha1.NodePrepStatus{Phase: v1alpha1.PhaseConfiguring},
	}
	a, dyn := retryTestAgent(t, np)
	for i := 1; i <= 15; i++ {
		a.backoffUntil = map[string]time.Time{}
		a.runStage(context.Background(), np, &v1alpha1.NodePrepProfile{})
		if p := currentPhase(t, dyn); p == string(v1alpha1.PhaseFailed) {
			t.Fatalf("a transient DNS failure exhausted the budget on iteration %d", i)
		}
	}
	if got := a.transientFailures["waitsForAPI"]; got != 15 {
		t.Fatalf("transient pacing counter = %d, want 15", got)
	}
	if len(np.Status.Steps) == 0 || np.Status.Steps[0].Attempts != 0 {
		t.Fatalf("transient failures must not increment the counted attempts: %+v", np.Status.Steps)
	}

	fail = false
	a.backoffUntil = map[string]time.Time{}
	a.runStage(context.Background(), np, &v1alpha1.NodePrepProfile{})
	if got := a.transientFailures["waitsForAPI"]; got != 0 {
		t.Fatalf("success must clear the transient pacing counter, got %d", got)
	}
	if len(np.Status.Steps) == 0 || np.Status.Steps[0].State != v1alpha1.StepDone {
		t.Fatalf("the step must complete once the API is back: %+v", np.Status.Steps)
	}
}
