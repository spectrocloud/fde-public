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

func cpTestNode(name string, tainted, labeled bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if labeled {
		n.Labels = map[string]string{"node-role.kubernetes.io/control-plane": ""}
	}
	if tainted {
		n.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}
	}
	return n
}

// A control plane registers with its taint before the role label; the agent
// must recognize both.
func TestNodeIsControlPlane(t *testing.T) {
	for _, c := range []struct {
		name             string
		tainted, labeled bool
		want             bool
	}{
		{"label", false, true, true},
		{"taint only", true, false, true},
		{"worker", false, false, false},
	} {
		a := &Agent{nodeName: "n1", client: k8sfake.NewSimpleClientset(cpTestNode("n1", c.tainted, c.labeled))}
		if got := a.nodeIsControlPlane(context.Background()); got != c.want {
			t.Errorf("%s: nodeIsControlPlane = %v, want %v", c.name, got, c.want)
		}
	}
}

// controlPlane.prep=false must freeze a retained NodePrep on the agent side
// too: the controller only stops new adoption.
func TestCycleHoldsControlPlaneWhenPrepDisabled(t *testing.T) {
	profilesGVR := schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "nodeprepprofiles"}
	orig := stepDefs
	t.Cleanup(func() { stepDefs = orig })
	ran := false
	stepDefs = []stepDef{{
		name: "mustNotRun", stage: v1alpha1.PhaseProvisioning,
		run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
			ran = true
			return v1alpha1.StepDone, "ran"
		},
	}}

	np := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": v1alpha1.GroupName + "/" + v1alpha1.Version,
		"kind":       "NodePrep",
		"metadata":   map[string]interface{}{"name": "cp-1"},
		"spec": map[string]interface{}{
			"nodeName":   "cp-1",
			"profileRef": map[string]interface{}{"name": "profile-1"},
		},
		"status": map[string]interface{}{
			"phase":                     string(v1alpha1.PhaseProvisioning),
			"bootId":                    "boot-1",
			"observedProfileGeneration": int64(1),
		},
	}}
	prof := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": v1alpha1.GroupName + "/" + v1alpha1.Version,
		"kind":       "NodePrepProfile",
		"metadata":   map[string]interface{}{"name": "profile-1", "generation": int64(1)},
		"spec": map[string]interface{}{
			"controlPlane": map[string]interface{}{"prep": false},
		},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodePrepsGVR: "NodePrepList", profilesGVR: "NodePrepProfileList"},
	)
	dyn.Tracker().Add(np)
	dyn.Tracker().Add(prof)

	node := cpTestNode("cp-1", true, false) // taint only: the registration race
	a := &Agent{nodeName: "cp-1", dyn: dyn, client: k8sfake.NewSimpleClientset(node)}
	a.execFn = func(_ []string, _ time.Duration, name string, _ bool, args []string) (string, error) {
		t.Fatalf("a prep-disabled control plane must not exec anything, got %s %v", name, args)
		return "", nil
	}

	a.cycle(context.Background(), "boot-1")
	if ran {
		t.Fatal("controlPlane.prep=false must hold the walk on a retained NodePrep")
	}
}
