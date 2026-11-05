package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// The waitForFipsKernel gate (1.1.0, Kevin): with the setting on and a
// non-FIPS kernel, cycle must perform no action at all — no inventory
// refresh, no boot hook, no walk, no boot-verify — and must not exec
// anything but the kernel probe. With the setting off, the kernel version
// stays irrelevant (standard behavior).
func TestCycleWaitsForFipsKernel(t *testing.T) {
	profilesGVR := schema.GroupVersionResource{
		Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "nodeprepprofiles",
	}
	orig := stepDefs
	t.Cleanup(func() { stepDefs = orig })

	for _, tc := range []struct {
		name    string
		wait    bool
		release string
		wantRun bool
	}{
		{"gate on, generic kernel: hold", true, "6.17.0-29-generic", false},
		{"gate on, FIPS kernel: proceed", true, "6.8.0-1015-fips", true},
		{"gate off, generic kernel: proceed", false, "6.17.0-29-generic", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			stepDefs = []stepDef{
				{name: "fakeWalk", stage: v1alpha1.PhaseProvisioning, critical: true,
					run: func(a *Agent, np *v1alpha1.NodePrep, p *v1alpha1.NodePrepProfile) (v1alpha1.StepState, string) {
						ran = true
						return v1alpha1.StepDone, "walked"
					}},
			}

			np := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": v1alpha1.GroupName + "/" + v1alpha1.Version,
				"kind":       "NodePrep",
				"metadata":   map[string]interface{}{"name": "node-1"},
				"spec": map[string]interface{}{
					"nodeName":   "node-1",
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
					"selection": map[string]interface{}{"waitForFipsKernel": tc.wait},
				},
			}}
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(),
				map[schema.GroupVersionResource]string{
					nodePrepsGVR: "NodePrepList",
					profilesGVR:  "NodePrepProfileList",
				},
			)
			dyn.Tracker().Add(np)
			dyn.Tracker().Add(prof)

			// A plain worker node: the CP admission gate fails closed on an
			// unreadable node, which would mask the FIPS gate's effect.
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
			a := &Agent{
				nodeName: "node-1",
				dyn:      dyn,
				client:   k8sfake.NewSimpleClientset(node),
			}
			var execs []string
			a.execFn = func(_ []string, _ time.Duration, name string, _ bool, args []string) (string, error) {
				execs = append(execs, name+" "+fmt.Sprint(args))
				if name == "uname" && len(args) == 1 && args[0] == "-r" {
					return tc.release + "\n", nil
				}
				t.Fatalf("a held cycle must not exec %s %v", name, args)
				return "", nil
			}

			a.cycle(context.Background(), "boot-1")
			if ran != tc.wantRun {
				t.Fatalf("walk ran=%v, want %v (execs: %v)", ran, tc.wantRun, execs)
			}
			if !tc.wantRun {
				if len(execs) != 1 || execs[0] != "uname [-r]" {
					t.Fatalf("a held cycle must only probe the kernel, got execs %v", execs)
				}
				// The hold is visible to an operator: one event with the reason.
				found := false
				for _, act := range a.client.(*k8sfake.Clientset).Actions() {
					ca, ok := act.(k8stesting.CreateAction)
					if act.GetVerb() != "create" || act.GetResource().Resource != "events" || !ok {
						continue
					}
					if ev, ok := ca.GetObject().(*corev1.Event); ok && ev.Reason == "WaitingForFipsKernel" {
						found = true
					}
				}
				if !found {
					t.Fatalf("a held cycle must emit a WaitingForFipsKernel event so the wait is observable")
				}
			}
		})
	}
}
