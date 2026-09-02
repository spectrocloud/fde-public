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

// probeMachineCRD is machineCRDExists without the log lines — for periodic
// re-probes, where a repeated "not found" would just be noise.
func probeMachineCRD(ctx context.Context, dyn dynamic.Interface) bool {
	_, err := dyn.Resource(machineGVR).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
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

// setMachinePause applies the pause annotation and reports whether the
// Machine's state actually changed. No-op when already paused.
func setMachinePause(ctx context.Context, dyn dynamic.Interface, m *unstructured.Unstructured) (bool, error) {
	if _, ok := m.GetAnnotations()[v1alpha1.CAPAPauseAnnotation]; ok {
		return false, nil
	}
	return true, patchMachineAnnotation(ctx, dyn, m, map[string]interface{}{v1alpha1.CAPAPauseAnnotation: ""})
}

// clearMachinePause removes the pause annotation and reports whether the
// Machine's state actually changed. No-op when not paused.
func clearMachinePause(ctx context.Context, dyn dynamic.Interface, m *unstructured.Unstructured) (bool, error) {
	if _, ok := m.GetAnnotations()[v1alpha1.CAPAPauseAnnotation]; !ok {
		return false, nil
	}
	return true, patchMachineAnnotation(ctx, dyn, m, map[string]interface{}{v1alpha1.CAPAPauseAnnotation: nil})
}

func patchMachineAnnotation(ctx context.Context, dyn dynamic.Interface, m *unstructured.Unstructured, ann map[string]interface{}) error {
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": ann},
	})
	if err != nil {
		return err
	}
	_, err = dyn.Resource(machineGVR).Namespace(m.GetNamespace()).
		Patch(ctx, m.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		fmt.Printf("[nodeprep] failed patching Machine %s/%s: %v\n", m.GetNamespace(), m.GetName(), err)
	}
	return err
}

// reconcileCAPAPause pauses the node's Machine while nodeprep owns it and
// unpauses at Ready. A Failed NodePrep stays paused: MachineHealthCheck
// recreating a half-flashed node is worse than a loud object (design §6.3).
// Every state change is described in the returned message so the caller can
// log it and emit an event — a pause that is applied or fails silently is
// invisible to exactly the operator debugging why CAPI stopped reconciling
// (found in live testing: the annotation was believed absent because
// nothing ever said it had been applied).
func reconcileCAPAPause(ctx context.Context, dyn dynamic.Interface, nodeName string, phase v1alpha1.Phase, paused bool) (string, error) {
	m, found := findMachineForNode(ctx, dyn, nodeName)
	if !found {
		return "", nil
	}
	where := fmt.Sprintf("Machine %s/%s", m.GetNamespace(), m.GetName())
	if paused {
		changed, err := setMachinePause(ctx, dyn, m)
		if err != nil {
			return "", fmt.Errorf("pausing %s: %v", where, err)
		}
		if changed {
			return where + " paused while nodeprep owns the node (phase " + string(phase) + ")", nil
		}
		return "", nil
	}
	changed, err := clearMachinePause(ctx, dyn, m)
	if err != nil {
		return "", fmt.Errorf("unpausing %s: %v", where, err)
	}
	if changed {
		return where + " unpaused: nodeprep reached Ready", nil
	}
	return "", nil
}

var kcpGVR = schema.GroupVersionResource{
	Group:    "controlplane.cluster.x-k8s.io",
	Version:  "v1beta1",
	Resource: "kubeadmcontrolplanes",
}

// kcpReplicas returns the KubeadmControlPlane replica count for the quorum
// math (design §6.4): the first KCP found with spec.replicas set — there is
// exactly one per cluster. Absent CRD or unset replicas is an error so the
// caller falls back to counting CP nodes.
func kcpReplicas(ctx context.Context, dyn dynamic.Interface) (int, error) {
	list, err := dyn.Resource(kcpGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	for i := range list.Items {
		r, found, err := unstructured.NestedInt64(list.Items[i].Object, "spec", "replicas")
		if err != nil {
			continue
		}
		if found && r > 0 {
			return int(r), nil
		}
	}
	return 0, fmt.Errorf("no KubeadmControlPlane with spec.replicas found")
}
