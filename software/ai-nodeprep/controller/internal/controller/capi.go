package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// CAPI Machine pause logic, absorbed from the legacy nodeprep-controller
// (software/nodeprep-controller/main.go) and now driven by NodePrep phases
// instead of bash label values (design §6.3).

var machineGVR = schema.GroupVersionResource{
	Group:    "cluster.x-k8s.io",
	Version:  "v1beta1",
	Resource: "machines",
}

// machineCRDExists mirrors the legacy controller's graceful detection: no
// Machine CRD (non-CAPI cluster) simply skips the pause logic.
func machineCRDExists(ctx context.Context, dyn dynamic.Interface) bool {
	_, err := dyn.Resource(machineGVR).List(ctx, metav1.ListOptions{Limit: 1})
	if err == nil {
		fmt.Println("[nodeprep] Machine CRD detected, CAPI pause logic active")
		return true
	}
	if errors.IsNotFound(err) {
		fmt.Println("[nodeprep] Machine CRD not found; CAPI pause logic inactive")
		return false
	}
	fmt.Printf("[nodeprep] unable to check Machine CRD, skipping CAPI pause logic: %v\n", err)
	return false
}

// findMachineForNode locates the CAPI Machine whose status.nodeRef.name is
// the node — same lookup as the legacy controller.
func findMachineForNode(ctx context.Context, dyn dynamic.Interface, nodeName string) (*unstructured.Unstructured, bool) {
	machines, err := dyn.Resource(machineGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Printf("[nodeprep] failed listing Machines: %v\n", err)
		return nil, false
	}
	for i := range machines.Items {
		m := &machines.Items[i]
		refName, found, err := unstructured.NestedString(m.Object, "status", "nodeRef", "name")
		if err != nil || !found || refName != nodeName {
			continue
		}
		return m, true
	}
	return nil, false
}

func setMachinePause(ctx context.Context, dyn dynamic.Interface, m *unstructured.Unstructured) {
	annotations := m.GetAnnotations()
	if annotations != nil {
		if _, ok := annotations[v1alpha1.CAPAPauseAnnotation]; ok {
			return
		}
	}
	patchMachineAnnotation(ctx, dyn, m, map[string]interface{}{v1alpha1.CAPAPauseAnnotation: ""})
}

func clearMachinePause(ctx context.Context, dyn dynamic.Interface, m *unstructured.Unstructured) {
	annotations := m.GetAnnotations()
	if annotations == nil {
		return
	}
	if _, ok := annotations[v1alpha1.CAPAPauseAnnotation]; !ok {
		return
	}
	patchMachineAnnotation(ctx, dyn, m, map[string]interface{}{v1alpha1.CAPAPauseAnnotation: nil})
}

func patchMachineAnnotation(ctx context.Context, dyn dynamic.Interface, m *unstructured.Unstructured, ann map[string]interface{}) {
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": ann},
	})
	if err != nil {
		return
	}
	_, err = dyn.Resource(machineGVR).Namespace(m.GetNamespace()).
		Patch(ctx, m.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		fmt.Printf("[nodeprep] failed patching Machine %s/%s: %v\n", m.GetNamespace(), m.GetName(), err)
	}
}

// reconcileCAPAPause pauses the node's Machine while nodeprep owns it and
// unpauses at Ready. A Failed NodePrep stays paused: MachineHealthCheck
// recreating a half-flashed node is worse than a loud object (design §6.3).
func reconcileCAPAPause(ctx context.Context, dyn dynamic.Interface, nodeName string, phase v1alpha1.Phase, paused bool) {
	m, found := findMachineForNode(ctx, dyn, nodeName)
	if !found {
		return
	}
	if paused {
		setMachinePause(ctx, dyn, m)
	} else {
		clearMachinePause(ctx, dyn, m)
	}
}
