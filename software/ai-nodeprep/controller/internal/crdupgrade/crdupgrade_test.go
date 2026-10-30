package crdupgrade

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// A fresh cluster has no CRDs: the job creates both.
func TestApplyCreatesMissingCRDs(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	names, err := Apply(context.Background(), dyn)
	if err != nil {
		t.Fatalf("apply on an empty cluster: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("expected both CRDs applied, got %v", names)
	}
	for _, name := range names {
		if _, err := dyn.Resource(crdGVR).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("CRD %s not created: %v", name, err)
		}
	}
}

// An existing cluster has the old schema and Helm ownership metadata. The
// job must replace the schema (the 1.1.0 waitForFipsKernel field) while
// keeping the annotations and labels another manager wrote — losing them
// would break the next Helm release's ownership checks.
func TestApplyUpdatesSchemaAndPreservesOwnershipMetadata(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata": map[string]interface{}{
			"name": "nodeprepprofiles.nodeprep.spectrocloud.com",
			"annotations": map[string]interface{}{
				"meta.helm.sh/release-name":      "nodeprep",
				"meta.helm.sh/release-namespace": "nodeprep-system",
			},
			"labels": map[string]interface{}{
				"app.kubernetes.io/managed-by": "Helm",
			},
		},
		"spec": map[string]interface{}{
			"group": "nodeprep.spectrocloud.com",
			"names": map[string]interface{}{"kind": "NodePrepProfile", "plural": "nodeprepprofiles"},
			"scope": "Cluster",
			// The pre-1.1.0 schema: no selection.waitForFipsKernel.
			"versions": []interface{}{map[string]interface{}{
				"name": "v1alpha1", "served": true, "storage": true,
				"schema": map[string]interface{}{"openAPIV3Schema": map[string]interface{}{"type": "object"}},
			}},
		},
	}}
	if err := dyn.Tracker().Add(existing); err != nil {
		t.Fatal(err)
	}

	if _, err := Apply(context.Background(), dyn); err != nil {
		t.Fatalf("apply over an existing CRD: %v", err)
	}

	got, err := dyn.Resource(crdGVR).Get(context.Background(), "nodeprepprofiles.nodeprep.spectrocloud.com", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ann := got.GetAnnotations()
	if ann["meta.helm.sh/release-name"] != "nodeprep" || ann["meta.helm.sh/release-namespace"] != "nodeprep-system" {
		t.Fatalf("Helm ownership annotations were lost: %v", ann)
	}
	if got.GetLabels()["app.kubernetes.io/managed-by"] != "Helm" {
		t.Fatalf("existing labels were lost: %v", got.GetLabels())
	}

	versions, found, err := unstructured.NestedSlice(got.Object, "spec", "versions")
	if err != nil || !found || len(versions) == 0 {
		t.Fatalf("applied CRD has no versions (found=%v err=%v)", found, err)
	}
	v0, ok := versions[0].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected version shape: %#v", versions[0])
	}
	field, found, err := unstructured.NestedMap(v0,
		"schema", "openAPIV3Schema", "properties", "spec", "properties", "selection", "properties", "waitForFipsKernel")
	if err != nil || !found {
		t.Fatalf("the applied schema must carry selection.waitForFipsKernel (found=%v err=%v)", found, err)
	}
	if field["type"] != "boolean" {
		t.Fatalf("waitForFipsKernel must be a boolean in the schema, got %#v", field)
	}
}
