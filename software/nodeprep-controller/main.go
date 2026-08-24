package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	labelKey        = "spectrocloud.com/nodeprep"
	taintKey        = "spectrocloud.com/nodeprep"
	pauseAnnotation = "cluster.x-k8s.io/paused"
	workerRoleLabel = "node-role.kubernetes.io/worker"
)

var machineGVR = schema.GroupVersionResource{
	Group:    "cluster.x-k8s.io",
	Version:  "v1beta1",
	Resource: "machines",
}

func machineCRDExists(ctx context.Context, dyn dynamic.Interface) bool {
	_, err := dyn.Resource(machineGVR).List(ctx, metav1.ListOptions{Limit: 1})
	if err == nil {
		log.Println("[nodeprep] Machine CRD detected")
		return true
	}

	if errors.IsNotFound(err) {
		log.Println("[nodeprep] Machine CRD not found; skipping Machine pause annotation logic")
		return false
	}

	log.Printf("[nodeprep] unable to check Machine CRD, skipping Machine pause annotation logic: %v", err)
	return false
}

func findMachineForNode(ctx context.Context, dyn dynamic.Interface, nodeName string) (*unstructured.Unstructured, bool) {
	machines, err := dyn.Resource(machineGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[nodeprep] failed listing Machines: %v", err)
		return nil, false
	}

	for _, machine := range machines.Items {
		refNodeName, found, err := unstructured.NestedString(
			machine.Object,
			"status",
			"nodeRef",
			"name",
		)
		if err != nil || !found || refNodeName != nodeName {
			continue
		}

		return &machine, true
	}

	return nil, false
}

func setMachinePauseAnnotation(ctx context.Context, dyn dynamic.Interface, machine *unstructured.Unstructured) {
	annotations := machine.GetAnnotations()
	if annotations != nil {
		if _, exists := annotations[pauseAnnotation]; exists {
			return
		}
	}

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				pauseAnnotation: "",
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		log.Printf("[nodeprep] failed marshaling pause patch for Machine %s/%s: %v",
			machine.GetNamespace(),
			machine.GetName(),
			err,
		)
		return
	}

	_, err = dyn.Resource(machineGVR).
		Namespace(machine.GetNamespace()).
		Patch(ctx, machine.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		log.Printf("[nodeprep] failed adding pause annotation to Machine %s/%s: %v",
			machine.GetNamespace(),
			machine.GetName(),
			err,
		)
		return
	}

	log.Printf("[nodeprep] added pause annotation to Machine %s/%s",
		machine.GetNamespace(),
		machine.GetName(),
	)
}

func removeMachinePauseAnnotation(ctx context.Context, dyn dynamic.Interface, machine *unstructured.Unstructured) {
	annotations := machine.GetAnnotations()
	if annotations == nil {
		return
	}

	if _, exists := annotations[pauseAnnotation]; !exists {
		return
	}

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				pauseAnnotation: nil,
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		log.Printf("[nodeprep] failed marshaling unpause patch for Machine %s/%s: %v",
			machine.GetNamespace(),
			machine.GetName(),
			err,
		)
		return
	}

	_, err = dyn.Resource(machineGVR).
		Namespace(machine.GetNamespace()).
		Patch(ctx, machine.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		log.Printf("[nodeprep] failed removing pause annotation from Machine %s/%s: %v",
			machine.GetNamespace(),
			machine.GetName(),
			err,
		)
		return
	}

	log.Printf("[nodeprep] removed pause annotation from Machine %s/%s",
		machine.GetNamespace(),
		machine.GetName(),
	)
}

func reconcileMachinePauseAnnotation(ctx context.Context, dyn dynamic.Interface, node *v1.Node) {
	labelValue, labelExists := node.Labels[labelKey]

	// Important: do nothing unless the nodeprep label key exists.
	if !labelExists {
		return
	}

	machine, found := findMachineForNode(ctx, dyn, node.Name)
	if !found {
		return
	}

	if labelValue == "complete" {
		removeMachinePauseAnnotation(ctx, dyn, machine)
		return
	}

	setMachinePauseAnnotation(ctx, dyn, machine)
}

func removeTaintIfComplete(ctx context.Context, c kubernetes.Interface, node *v1.Node) {
	if node.Labels[labelKey] != "complete" {
		return
	}

	found := false
	for _, t := range node.Spec.Taints {
		if t.Key == taintKey && t.Effect == v1.TaintEffectNoSchedule {
			found = true
			break
		}
	}

	if !found {
		return
	}

	for i := 0; i < 3; i++ {
		n, err := c.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			log.Printf("[nodeprep] failed getting node %s: %v", node.Name, err)
			return
		}

		newTaints := make([]v1.Taint, 0, len(n.Spec.Taints))
		for _, t := range n.Spec.Taints {
			if !(t.Key == taintKey && t.Effect == v1.TaintEffectNoSchedule) {
				newTaints = append(newTaints, t)
			}
		}

		if len(newTaints) == len(n.Spec.Taints) {
			return
		}

		n.Spec.Taints = newTaints

		_, err = c.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		if err == nil {
			log.Printf("[nodeprep] removed taint from node %s", n.Name)
			return
		}

		log.Printf("[nodeprep] failed updating node %s, retrying: %v", n.Name, err)
		time.Sleep(200 * time.Millisecond)
	}
}

func removeWorkerRoleLabel(ctx context.Context, c kubernetes.Interface, node *v1.Node) {
	if _, exists := node.Labels[workerRoleLabel]; !exists {
		return
	}

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				workerRoleLabel: nil,
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		log.Printf("[nodeprep] failed marshaling worker-role removal patch for node %s: %v", node.Name, err)
		return
	}

	_, err = c.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		log.Printf("[nodeprep] failed removing worker-role label from node %s: %v", node.Name, err)
		return
	}

	log.Printf("[nodeprep] removed worker-role label from node %s", node.Name)
}

func addWorkerRoleLabel(ctx context.Context, c kubernetes.Interface, node *v1.Node) {
	if _, exists := node.Labels[workerRoleLabel]; exists {
		return
	}

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				workerRoleLabel: "",
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		log.Printf("[nodeprep] failed marshaling worker-role addition patch for node %s: %v", node.Name, err)
		return
	}

	_, err = c.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		log.Printf("[nodeprep] failed adding worker-role label to node %s: %v", node.Name, err)
		return
	}

	log.Printf("[nodeprep] added worker-role label to node %s", node.Name)
}

func reconcileWorkerRoleLabel(ctx context.Context, c kubernetes.Interface, node *v1.Node) {
	labelValue, labelExists := node.Labels[labelKey]

	// Important: do nothing unless the nodeprep label key exists.
	if !labelExists {
		return
	}

	switch labelValue {
	case "precomplete":
		removeWorkerRoleLabel(ctx, c, node)
	case "complete":
		addWorkerRoleLabel(ctx, c, node)
	}
}

func handleNode(
	ctx context.Context,
	client kubernetes.Interface,
	dyn dynamic.Interface,
	node *v1.Node,
	machineCRDAvailable bool,
) {
	if machineCRDAvailable {
		reconcileMachinePauseAnnotation(ctx, dyn, node)
	}

	removeTaintIfComplete(ctx, client, node)
	reconcileWorkerRoleLabel(ctx, client, node)
}

func main() {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("[nodeprep] failed loading in-cluster config: %v", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("[nodeprep] failed creating kubernetes client: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("[nodeprep] failed creating dynamic client: %v", err)
	}

	factory := informers.NewSharedInformerFactory(client, 0)
	nodeInformer := factory.Core().V1().Nodes().Informer()

	ctx := context.Background()

	machineCRDAvailable := machineCRDExists(ctx, dynClient)

	nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*v1.Node)
			if !ok {
				return
			}
			handleNode(ctx, client, dynClient, node, machineCRDAvailable)
		},
		UpdateFunc: func(_, newObj interface{}) {
			node, ok := newObj.(*v1.Node)
			if !ok {
				return
			}
			handleNode(ctx, client, dynClient, node, machineCRDAvailable)
		},
	})

	log.Println("[nodeprep] starting node informer")

	stop := make(chan struct{})
	defer close(stop)

	nodeInformer.Run(stop)
}
