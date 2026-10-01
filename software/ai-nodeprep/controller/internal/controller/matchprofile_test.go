package controller

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// profileU builds an unstructured NodePrepProfile with the given spec.policy.
func profileU(name string, policy map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": v1alpha1.GroupName + "/" + v1alpha1.Version,
		"kind":       v1alpha1.NodePrepProfileKind,
		"metadata":   map[string]interface{}{"name": name},
		"spec": map[string]interface{}{
			"selection": map[string]interface{}{"mode": "allNodes"},
			"policy":    policy,
		},
	}}
}

// captureStdout redirects os.Stdout to a pipe and returns what was written
// during fn (matchProfile logs through fmt.Printf).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

// The 0.1.91 rollout left the stored labelCompat as the string "v1" while the
// CRD had already become boolean: matchProfile skipped the object silently,
// so both nodes logged "no longer matches any profile" every 30s resync with
// no hint why. An undecodable profile must be reported — once per name, not
// once per resync — and decodable profiles must still match.
func TestMatchProfileDecodeFailureLoggedOnce(t *testing.T) {
	ctx := context.Background()
	stale := profileU("stale-profile", map[string]interface{}{"labelCompat": "v1"})
	good := profileU("good-profile", map[string]interface{}{"labelCompat": false})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{profilesGVR: "NodePrepProfileList"},
		stale, good)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{}}}
	c := New(nil, dyn, "nodeprep-system")

	// Two rounds (the informer resync): the warning is logged once, not per
	// call, and the decodable profile still matches the node.
	out := captureStdout(t, func() {
		for round := 0; round < 2; round++ {
			p, err := c.matchProfile(ctx, node)
			if err != nil {
				t.Fatalf("round %d: matchProfile: %v", round, err)
			}
			if p == nil || p.Name != "good-profile" {
				t.Fatalf("round %d: matched %v, want good-profile", round, p)
			}
		}
	})
	if n := strings.Count(out, "stale-profile failed to decode"); n != 1 {
		t.Fatalf("want exactly one decode warning for stale-profile, got %d in:\n%s", n, out)
	}
	if strings.Contains(out, "good-profile failed to decode") {
		t.Fatalf("the decodable profile must not be reported:\n%s", out)
	}
}
