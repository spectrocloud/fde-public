// Package controller implements the NodePrep cluster controller: node
// adoption via NodePrepProfile selectors, the taint contract, CAPI pause,
// worker-role label management, the legacy label mirror, and the fleet flash
// window (design NP-CTRL-001 §4, §6, §9).
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"spectrocloud.com/nodeprep/api/v1alpha1"
	"spectrocloud.com/nodeprep/internal/k8sutil"
	"spectrocloud.com/nodeprep/internal/phases"
)

var (
	profilesGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "nodeprepprofiles"}
	nodePrepsGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "nodepreps"}
)

// Controller is the cluster controller. v0.1 runs a single replica without
// leader election (documented in the README).
type Controller struct {
	client kubernetes.Interface
	dyn    dynamic.Interface
	// ns is where Events are recorded and the informer-based components live.
	ns         string
	nodeIndexer cache.Indexer
	machineCRD bool
}

func New(client kubernetes.Interface, dyn dynamic.Interface, ns string) *Controller {
	return &Controller{client: client, dyn: dyn, ns: ns}
}

// Run starts the node informer and reconciles on every add/update/resync.
func (c *Controller) Run(ctx context.Context) error {
	c.machineCRD = machineCRDExists(ctx, c.dyn)

	factory := informers.NewSharedInformerFactory(c.client, 30*time.Second)
	nodeInf := factory.Core().V1().Nodes().Informer()
	nodeInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.enqueue(ctx, obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			c.enqueue(ctx, newObj)
		},
	})
	c.nodeIndexer = nodeInf.GetIndexer()

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), nodeInf.HasSynced) {
		return fmt.Errorf("node informer cache sync failed")
	}
	<-ctx.Done()
	return nil
}

func (c *Controller) enqueue(ctx context.Context, obj interface{}) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	// Reconciles are cheap (small clusters, cached reads); run them inline
	// behind the informer's delivery. A workqueue is warranted when the
	// flash window and CP admission see production scale.
	c.reconcileNode(ctx, node.Name)
}

// matchProfile returns the first profile whose nodeSelector matches the node
// (deterministic by name order). nil when no profile claims the node.
func (c *Controller) matchProfile(ctx context.Context, node *corev1.Node) (*v1alpha1.NodePrepProfile, error) {
	list, err := c.dyn.Resource(profilesGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	byName := map[string]*v1alpha1.NodePrepProfile{}
	for i := range list.Items {
		p := &v1alpha1.NodePrepProfile{}
		if err := decodeInto(&list.Items[i], p); err != nil {
			continue
		}
		names = append(names, p.Name)
		byName[p.Name] = p
	}
	sortStrings(names)
	for _, name := range names {
		p := byName[name]
		if p.Spec.NodeSelector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(p.Spec.NodeSelector)
		if err != nil {
			continue
		}
		if sel.Matches(labels.Set(node.Labels)) {
			return p, nil
		}
	}
	return nil, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (c *Controller) getNodePrep(ctx context.Context, nodeName string) (*v1alpha1.NodePrep, error) {
	u, err := c.dyn.Resource(nodePrepsGVR).Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	np := &v1alpha1.NodePrep{}
	if err := decodeInto(u, np); err != nil {
		return nil, err
	}
	return np, nil
}

func (c *Controller) reconcileNode(ctx context.Context, nodeName string) {
	defer utilruntime.HandleCrash()

	var node *corev1.Node
	if obj, exists, err := c.nodeIndexer.GetByKey(nodeName); err != nil || !exists {
		return
	} else {
		node = obj.(*corev1.Node)
	}

	profile, err := c.matchProfile(ctx, node)
	if err != nil {
		fmt.Printf("[nodeprep] profile match failed for %s: %v\n", nodeName, err)
		return
	}

	np, err := c.getNodePrep(ctx, nodeName)
	if err != nil {
		np = nil
	}

	if profile == nil {
		if np != nil {
			// De-adopted: leave the object for the operator (design §10);
			// it stays as the audit record of what ran.
			fmt.Printf("[nodeprep] node %s no longer matches any profile; NodePrep retained\n", nodeName)
		}
		return
	}

	isCP := isControlPlane(node)
	if isCP && !profile.Spec.Policy.ControlPlanePrep {
		return
	}

	if np == nil {
		np, err = c.adoptNode(ctx, node, profile)
		if err != nil {
			fmt.Printf("[nodeprep] adoption failed for %s: %v\n", nodeName, err)
			return
		}
	} else if np.Spec.ProfileRef.Name != profile.Name {
		// Profile reassignment: patch the spec reference.
		if err := c.patchProfileRef(ctx, np.Name, profile.Name); err != nil {
			fmt.Printf("[nodeprep] profile reassignment failed for %s: %v\n", nodeName, err)
			return
		}
		np.Spec.ProfileRef.Name = profile.Name
	}

	c.lifecycle(ctx, node, profile, np, isCP)
}

// adoptNode creates the NodePrep object, importing the bash state label when
// present (design §10 import step).
func (c *Controller) adoptNode(ctx context.Context, node *corev1.Node, profile *v1alpha1.NodePrepProfile) (*v1alpha1.NodePrep, error) {
	phase := v1alpha1.PhasePending
	legacy, hasLegacy := node.Labels[v1alpha1.LegacyLabel]
	if hasLegacy {
		p, err := phases.FromLegacy(legacy)
		if err != nil {
			k8sutil.Emit(ctx, c.client, c.ns, v1alpha1.NodePrepKind, node.Name, corev1.EventTypeWarning, "UnknownLegacyState",
				fmt.Sprintf("legacy label %s=%q is unknown; starting from Pending", v1alpha1.LegacyLabel, legacy))
		} else {
			phase = p
		}
	}

	np := &v1alpha1.NodePrep{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupName + "/" + v1alpha1.Version, Kind: v1alpha1.NodePrepKind},
		ObjectMeta: metav1.ObjectMeta{
			Name: node.Name,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1.SchemeGroupVersion.String(),
				Kind:       "Node",
				Name:       node.Name,
				UID:        node.UID,
			}},
		},
		Spec: v1alpha1.NodePrepSpec{NodeName: node.Name, ProfileRef: v1alpha1.ProfileRef{Name: profile.Name}},
		Status: v1alpha1.NodePrepStatus{
			Phase:                     phase,
			ObservedProfileGeneration: profile.Generation,
		},
	}
	raw, err := json.Marshal(np)
	if err != nil {
		return nil, err
	}
	u, _, err := unstructured.UnstructuredJSONScheme.Decode(raw, nil, nil)
	if err != nil {
		return nil, err
	}
	created, err := c.dyn.Resource(nodePrepsGVR).Create(ctx, u.(*unstructured.Unstructured), metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("adopted by profile %s at phase %s", profile.Name, phase)
	if hasLegacy {
		msg += fmt.Sprintf(" (imported from legacy label %s=%q)", v1alpha1.LegacyLabel, legacy)
	}
	k8sutil.Emit(ctx, c.client, c.ns, v1alpha1.NodePrepKind, node.Name, corev1.EventTypeNormal, "Adopted", msg)
	fmt.Printf("[nodeprep] %s\n", msg)
	return createdToNodePrep(created)
}

func createdToNodePrep(u *unstructured.Unstructured) (*v1alpha1.NodePrep, error) {
	np := &v1alpha1.NodePrep{}
	err := decodeInto(u, np)
	return np, err
}

func (c *Controller) patchProfileRef(ctx context.Context, name, profile string) error {
	patch, _ := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"profileRef": map[string]string{"name": profile}},
	})
	_, err := c.dyn.Resource(nodePrepsGVR).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// lifecycle applies the node-facing side of the design: taint contract,
// worker-role label, legacy mirror label, CAPI pause, flash window and CP
// admission conditions, and the Ready condition.
func (c *Controller) lifecycle(ctx context.Context, node *corev1.Node, profile *v1alpha1.NodePrepProfile, np *v1alpha1.NodePrep, isCP bool) {
	phase := np.Status.Phase
	if phase == "" {
		phase = v1alpha1.PhasePending
	}
	pol := profile.Spec.Policy
	bootVerified := k8sutil.ConditionStatus(np.Status.Conditions, v1alpha1.ConditionBootVerified) == "True"

	// --- Node object: taint + labels (single update with retry) ---
	labelsWant := map[string]string{}
	if pol.LabelCompat != "off" {
		if v, err := phases.LegacyFor(phase); err == nil {
			labelsWant[v1alpha1.LegacyLabel] = v
		}
	}
	switch WorkerLabelDecision(phase, pol) {
	case WorkerLabelSet:
		labelsWant[v1alpha1.WorkerRoleLabel] = ""
	case WorkerLabelRemove:
		labelsWant[v1alpha1.WorkerRoleLabel] = "\x00delete"
	}
	wantTaint := TaintShouldExist(phase, bootVerified, pol)
	c.applyNodeChanges(ctx, node, wantTaint, labelsWant, phase, np.Name)

	// --- CAPI pause ---
	if pol.CAPause && c.machineCRD {
		reconcileCAPAPause(ctx, c.dyn, node.Name, phase, phase != v1alpha1.PhaseReady)
	}

	// --- Conditions ---
	conds := np.Status.Conditions
	if changed := c.refreshAdmissionConditions(ctx, np, phase, pol, isCP, &conds); changed {
		c.patchConditions(ctx, np.Name, conds)
	}
	// Ready mirrors the phase; reasons name what is holding it back.
	readyStatus, readyReason, readyMsg := readyFor(phase, np)
	if k8sutil.SetCondition(&conds, v1alpha1.ConditionReady, readyStatus, readyReason, readyMsg, np.Status.ObservedProfileGeneration) {
		c.patchConditions(ctx, np.Name, conds)
	}
}

// applyNodeChanges updates the Node with the desired taint and label state,
// with a small retry loop against update conflicts (same pattern as the
// legacy controller). Events fire on taint transitions.
func (c *Controller) applyNodeChanges(ctx context.Context, node *corev1.Node, wantTaint bool, labelsWant map[string]string, phase v1alpha1.Phase, npName string) {
	// Fast path on the informer copy; the retry loop re-reads before writing.
	taintChanged := k8sutil.HasTaint(node, v1alpha1.TaintKey) != wantTaint
	labelsChanged := false
	for k, v := range labelsWant {
		cur, ok := node.Labels[k]
		if v == "\x00delete" {
			if ok {
				labelsChanged = true
			}
			continue
		}
		if !ok || cur != v {
			labelsChanged = true
		}
	}
	if !taintChanged && !labelsChanged {
		return
	}
	for i := 0; i < 3; i++ {
		n, err := c.client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			fmt.Printf("[nodeprep] failed getting node %s: %v\n", node.Name, err)
			return
		}
		if !k8sutil.ApplyNodeChanges(n, wantTaint, v1alpha1.TaintKey, labelsWant) {
			return
		}
		_, err = c.client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		if err == nil {
			if k8sutil.HasTaint(n, v1alpha1.TaintKey) && !k8sutil.HasTaint(node, v1alpha1.TaintKey) {
				k8sutil.Emit(ctx, c.client, c.ns, v1alpha1.NodePrepKind, npName, corev1.EventTypeNormal, "TaintApplied",
					"nodeprep taint held: node is owned by nodeprep")
			} else if !k8sutil.HasTaint(n, v1alpha1.TaintKey) && k8sutil.HasTaint(node, v1alpha1.TaintKey) {
				k8sutil.Emit(ctx, c.client, c.ns, v1alpha1.NodePrepKind, npName, corev1.EventTypeNormal, "TaintReleased",
					fmt.Sprintf("nodeprep taint released: phase %s with boot verified", phase))
			}
			return
		}
		fmt.Printf("[nodeprep] failed updating node %s, retrying: %v\n", node.Name, err)
		time.Sleep(200 * time.Millisecond)
	}
}

// refreshAdmissionConditions computes FlashAdmitted and (for control planes)
// MaintenanceAdmitted from the fleet state. Returns true when conditions changed.
func (c *Controller) refreshAdmissionConditions(ctx context.Context, np *v1alpha1.NodePrep, phase v1alpha1.Phase, pol v1alpha1.PolicySpec, isCP bool, conds *[]metav1.Condition) bool {
	changed := false
	maxFlash := pol.MaxConcurrentFlashes
	busyFlash := c.countBusyFlashers(ctx, np.Name)
	if phase == v1alpha1.PhaseProvisioning || phase == v1alpha1.PhaseFlashing {
		if AdmitFlashing(busyFlash, maxFlash) {
			changed = k8sutil.SetCondition(conds, v1alpha1.ConditionFlashAdmitted, "True", v1alpha1.ReasonAdmitted,
				fmt.Sprintf("%d other node(s) flashing", busyFlash), 0) || changed
		} else {
			changed = k8sutil.SetCondition(conds, v1alpha1.ConditionFlashAdmitted, "False", v1alpha1.ReasonWindowFull,
				fmt.Sprintf("flash window full: %d/%d nodes flashing", busyFlash, maxFlash), 0) || changed
		}
	}
	if isCP {
		busyCP := c.countBusyControlPlanes(ctx, np.Name)
		if AdmitControlPlane(busyCP, "") {
			changed = k8sutil.SetCondition(conds, v1alpha1.ConditionMaintenanceAdmitted, "True", v1alpha1.ReasonAdmitted,
				"serial control-plane window available", 0) || changed
		} else {
			changed = k8sutil.SetCondition(conds, v1alpha1.ConditionMaintenanceAdmitted, "False", v1alpha1.ReasonQuorumFloor,
				"another control-plane node is mid-prep; quorum floor is one member short", 0) || changed
		}
	}
	return changed
}

func (c *Controller) countBusyFlashers(ctx context.Context, exclude string) int {
	return c.countInPhase(ctx, exclude, v1alpha1.PhaseFlashing)
}

func (c *Controller) countBusyControlPlanes(ctx context.Context, exclude string) int {
	list, err := c.dyn.Resource(nodePrepsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0
	}
	n := 0
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetName() == exclude {
			continue
		}
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		if v1alpha1.Phase(phase) == v1alpha1.PhasePending ||
			v1alpha1.Phase(phase) == v1alpha1.PhaseProvisioning ||
			v1alpha1.Phase(phase) == v1alpha1.PhaseFlashing ||
			v1alpha1.Phase(phase) == v1alpha1.PhaseConfiguring ||
			v1alpha1.Phase(phase) == v1alpha1.PhaseFinalizing {
			if node, ok, _ := c.nodeIndexer.GetByKey(item.GetName()); ok {
				if isControlPlane(node.(*corev1.Node)) {
					n++
				}
			}
		}
	}
	return n
}

func (c *Controller) countInPhase(ctx context.Context, exclude string, phase v1alpha1.Phase) int {
	list, err := c.dyn.Resource(nodePrepsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0
	}
	n := 0
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetName() == exclude {
			continue
		}
		p, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		if v1alpha1.Phase(p) == phase {
			n++
		}
	}
	return n
}

func (c *Controller) patchConditions(ctx context.Context, name string, conds []metav1.Condition) {
	patch, err := json.Marshal(map[string]interface{}{"status": map[string]interface{}{"conditions": conds}})
	if err != nil {
		return
	}
	_, err = c.dyn.Resource(nodePrepsGVR).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}, "status")
	if err != nil {
		fmt.Printf("[nodeprep] failed patching conditions on %s: %v\n", name, err)
	}
}

// readyFor derives the Ready condition from the phase and step states.
func readyFor(phase v1alpha1.Phase, np *v1alpha1.NodePrep) (status, reason, message string) {
	switch phase {
	case v1alpha1.PhaseReady:
		return "True", v1alpha1.ReasonVerified, "node prepared and boot verified"
	case v1alpha1.PhaseFailed:
		return "False", v1alpha1.ReasonStepFailed, "a step exhausted its retry budget; see events and status.steps"
	default:
		for _, s := range np.Status.Steps {
			if s.State == v1alpha1.StepBlocked {
				return "False", v1alpha1.ReasonStepsBlocked, fmt.Sprintf("step %s blocked: %s", s.Name, s.Message)
			}
			if s.State == v1alpha1.StepFailed {
				return "False", v1alpha1.ReasonStepFailed, fmt.Sprintf("step %s failed: %s", s.Name, s.Message)
			}
		}
		return "False", v1alpha1.ReasonConverging, fmt.Sprintf("nodeprep converging at phase %s", phase)
	}
}

func isControlPlane(node *corev1.Node) bool {
	if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}
	_, ok := node.Labels["node-role.kubernetes.io/master"]
	return ok
}

// decodeInto converts an unstructured object into a typed struct via JSON.
func decodeInto(u *unstructured.Unstructured, out interface{}) error {
	raw, err := json.Marshal(u.Object)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
