// Package crdupgrade applies the controller's embedded CRD manifests.
//
// The Helm chart runs this as a pre-upgrade hook job so an existing cluster
// picks up a NodePrepProfile schema change by deploying the newer chart,
// without a manual kubectl apply. The job is idempotent: it creates a CRD
// that does not exist and updates one that does.
package crdupgrade

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"

	"spectrocloud.com/nodeprep/manifests"
)

// crdGVR is the apiextensions v1 CustomResourceDefinition collection.
var crdGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// Apply creates or updates every embedded CRD, in manifest order. It
// returns the names it touched, including those applied before a later
// failure, so the job log shows how far it got.
func Apply(ctx context.Context, dyn dynamic.Interface) ([]string, error) {
	var applied []string
	for _, raw := range manifests.CRDs() {
		name, err := applyOne(ctx, dyn, raw)
		if name != "" {
			applied = append(applied, name)
		}
		if err != nil {
			return applied, err
		}
	}
	return applied, nil
}

// applyOne decodes one CRD document and creates or updates it. The update
// keeps the live object's identity and the ownership metadata another
// manager (Helm) wrote — annotations and labels such as meta.helm.sh/* —
// because only the schema is this job's to own. Dropping them would break
// the next Helm release's ownership checks.
func applyOne(ctx context.Context, dyn dynamic.Interface, raw []byte) (string, error) {
	var obj map[string]interface{}
	if err := sigsyaml.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("decode embedded manifest: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	if u.GetKind() != "CustomResourceDefinition" {
		return "", fmt.Errorf("embedded manifest %q is a %s, not a CustomResourceDefinition", u.GetName(), u.GetKind())
	}
	name := u.GetName()

	cur, err := dyn.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := dyn.Resource(crdGVR).Create(ctx, u, metav1.CreateOptions{}); err != nil {
			return name, fmt.Errorf("create CRD %s: %w", name, err)
		}
		return name, nil
	case err != nil:
		return name, fmt.Errorf("get CRD %s: %w", name, err)
	}

	u.SetResourceVersion(cur.GetResourceVersion())
	u.SetUID(cur.GetUID())
	u.SetAnnotations(mergeStringMap(cur.GetAnnotations(), u.GetAnnotations()))
	u.SetLabels(mergeStringMap(cur.GetLabels(), u.GetLabels()))
	if _, err := dyn.Resource(crdGVR).Update(ctx, u, metav1.UpdateOptions{}); err != nil {
		return name, fmt.Errorf("update CRD %s: %w", name, err)
	}
	return name, nil
}

// mergeStringMap overlays onto base in place; a nil base takes overlay as-is.
func mergeStringMap(base, overlay map[string]string) map[string]string {
	if base == nil {
		return overlay
	}
	for k, v := range overlay {
		base[k] = v
	}
	return base
}
