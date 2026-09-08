// Package v1alpha1 defines the NodePrepProfile and NodePrep APIs.
//
// Design reference: software/ai-nodeprep/design/nodeprep-controller-design.html (NP-CTRL-001).
// The structs are plain Go types (no scheme registration): objects are read and
// written through the dynamic client, so no deepcopy code is needed in v0.1.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	GroupName = "nodeprep.spectrocloud.com"
	Version   = "v1alpha1"

	// NodePrepProfileKind / NodePrepKind are the cluster-scoped kinds.
	NodePrepProfileKind = "NodePrepProfile"
	NodePrepKind        = "NodePrep"

	// LegacyLabel is the state label the bash script used. The controller
	// keeps mirroring it (policy.labelCompat: v1) so existing tooling keeps
	// working during migration.
	LegacyLabel = "spectrocloud.com/nodeprep"
	// TaintKey is held while nodeprep owns the node (design §6.1).
	TaintKey = "spectrocloud.com/nodeprep"
	// WorkerRoleLabel is demoted entering Finalizing, restored at Ready (design §6.3).
	WorkerRoleLabel = "node-role.kubernetes.io/worker"
	// ControlPlaneTaintKey is kubeadm's CP taint; its absence marks a CP
	// node as expected to execute workloads, so the worker-role label
	// choreography reaches it too (see WorkerLabelApplies).
	ControlPlaneTaintKey = "node-role.kubernetes.io/control-plane"
	// ResumeAnnotation restarts a Failed NodePrep (design §5.2).
	ResumeAnnotation = "nodeprep.spectrocloud.com/resume"
	// CAPAPauseAnnotation pauses the CAPI Machine owning a node (design §6.3).
	CAPAPauseAnnotation = "cluster.x-k8s.io/paused"
)

// Phase is the coarse lifecycle state (design §5.2, figure 3). The names map
// one-to-one onto the bash script's label values via phases.LegacyFor / phases.FromLegacy.
type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseProvisioning Phase = "Provisioning"
	PhaseFlashing     Phase = "Flashing"
	PhaseConfiguring  Phase = "Configuring"
	PhaseFinalizing   Phase = "Finalizing"
	PhaseReady        Phase = "Ready"
	PhaseFailed       Phase = "Failed"

	// PhaseColdRebootRequired overlays the walk while the SR-IOV NV cold
	// halt waits for the operator's manual power cycle (0.1.54): the walk
	// underneath is still Finalizing and keeps running (only its step
	// bodies can observe the post-power-cycle convergence), but
	// .status.phase must say at a glance that manual action is required.
	// It applies only mid-walk in Finalizing — the only stage whose steps
	// raise the halt — and lifts back to Finalizing when the
	// ColdRebootRequired condition clears.
	PhaseColdRebootRequired Phase = "ColdRebootRequired"
)

// Step states (design §5.1).
type StepState string

const (
	StepPending    StepState = "Pending"
	StepInProgress StepState = "InProgress"
	StepDone       StepState = "Done"
	StepBlocked    StepState = "Blocked"
	StepFailed     StepState = "Failed"
)

// Condition types (design §3.4).
const (
	ConditionReady               = "Ready"
	ConditionRebootRequired      = "RebootRequired"
	ConditionBootVerified        = "BootVerified"
	ConditionFlashAdmitted       = "FlashAdmitted"
	ConditionMaintenanceAdmitted = "MaintenanceAdmitted"
	// ConditionColdRebootRequired is set True when the firmware has
	// committed the requested SR-IOV NV but a warm reboot did not expose
	// it (sriov_totalvfs still short): the walk halts its reboot cycle and
	// waits for the operator to power the node down and back up (0.1.51).
	ConditionColdRebootRequired = "ColdRebootRequired"
)

// Condition reasons.
const (
	ReasonConverging    = "Converging"   // steps still running in the current stage
	ReasonStepsBlocked  = "StepsBlocked" // a step needs operator action or host tools
	ReasonStepFailed    = "StepFailed"   // retry budget exhausted on a step
	ReasonAdmitted      = "Admitted"
	ReasonWindowFull    = "WindowFull"
	ReasonQuorumFloor   = "QuorumFloor"
	ReasonVerified      = "Verified"
	ReasonPending       = "Pending"
	ReasonDriftDetected = "DriftDetected"
)

// RebootRequired reasons (design §5.2).
const (
	RebootDocaInstalled    = "DocaInstalled"
	RebootBFBFlashed       = "BFBFlashed"
	RebootMlxConfigApplied = "MlxConfigApplied"
	RebootGrubChanged      = "GrubChanged"
	RebootIbCoreNetns      = "IbCoreNetns"
)

// +separate doc
type ProfileRef struct {
	Name string `json:"name"`
}

type NodePrepProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodePrepProfileSpec `json:"spec"`
}

type NodePrepProfileSpec struct {
	// Selection decides which Nodes are adopted (§3.1) — the single
	// selection control. When unset, no nodes are adopted: a profile must
	// declare how it picks its nodes.
	// +optional
	Selection *SelectionSpec `json:"selection,omitempty"`

	Firmware     FirmwareSource   `json:"firmware,omitempty"`
	EastWest     EastWestSpec     `json:"eastWest,omitempty"`
	NorthSouth   NorthSouthSpec   `json:"northSouth,omitempty"`
	Rails        []Rail           `json:"rails,omitempty"`
	HostBoot     HostBootSpec     `json:"hostBoot,omitempty"`
	Policy       PolicySpec       `json:"policy,omitempty"`
	DPUBMC       DPUBMCSpec       `json:"dpuBMC,omitempty"`
	NFSRDMA      NFSRDMASpec      `json:"nfsRdma,omitempty"`
	ControlPlane ControlPlaneSpec `json:"controlPlane,omitempty"`
}

// SelectionSpec picks the node set a profile claims. Mode labelSelector
// (default) gates on a label selector; allWorkers adopts every non-control-
// plane node; allNodes adopts workers and control planes alike (CP prep
// still requires policy.controlPlanePrep and follows the §6.4 quorum
// choreography). ExcludeLabel disqualifies individual nodes under every
// mode — an escape hatch for a node that must never be touched.
type SelectionSpec struct {
	// Mode picks how nodes are selected: "" / "labelSelector" (gate on
	// NodeSelector), "allWorkers" (every node without a control-plane
	// role), "allNodes" (workers and control planes).
	// +kubebuilder:validation:Enum=labelSelector;allWorkers;allNodes
	Mode string `json:"mode,omitempty"`
	// NodeSelector is the label gate for mode labelSelector. Optional
	// there too: an empty selector matches every node.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
	// ExcludeLabel disqualifies nodes carrying it, in any mode: "key"
	// matches any value, "key=value" an exact one.
	// +optional
	ExcludeLabel string `json:"excludeLabel,omitempty"`
}

// Nil-safe accessors: a nil Selection adopts nothing.

func (s *SelectionSpec) GetMode() string {
	if s == nil {
		return ""
	}
	return s.Mode
}

func (s *SelectionSpec) GetNodeSelector() *metav1.LabelSelector {
	if s == nil {
		return nil
	}
	return s.NodeSelector
}

func (s *SelectionSpec) GetExcludeLabel() string {
	if s == nil {
		return ""
	}
	return s.ExcludeLabel
}

type FirmwareSource struct {
	Source     string     `json:"source,omitempty"` // e.g. http://maas.internal:8069/rcp
	BFB        BFBSource  `json:"bfb,omitempty"`
	DOCA       DOCASource `json:"doca,omitempty"`
	AptUpgrade bool       `json:"aptUpgrade,omitempty"`
}

type BFBSource struct {
	Name string `json:"name,omitempty"`
	// MinVersion is the firmware floor for the BFB flash gate (design §8.2,
	// bash BFB_FW + vercomp): a BlueField-3 whose running firmware (flint q,
	// the MFT enrichment's fwVer) sorts below this is outdated and is flashed
	// with the configured BFB; one at or above it needs no flash and no
	// reboot. Dot-separated numeric comparison with the bash vercomp's
	// zero-padding (a missing field counts as 0, so "32.49" < "32.49.1014");
	// ConnectX-class adapters take their firmware from the DOCA install, not
	// the BFB, and are never version-compared. Empty = no version gate: the
	// flash decision falls to the flash body alone.
	MinVersion string `json:"minVersion,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}

type DOCASource struct {
	Deb    string `json:"deb,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Packages installs with the deb's apt transaction. The one supported
	// shell-style placeholder is $(uname -r), expanded by the agent from
	// the host kernel release (e.g. linux-headers-$(uname -r)); any other
	// shell expression is a step error, never a pass-through.
	Packages []string `json:"packages,omitempty"`
}

type EastWestSpec struct {
	LinkType    string `json:"linkType,omitempty"`    // InfiniBand | Ethernet (bash: LINKTYPE_EW 1|2)
	NumVFs      int    `json:"numVFs,omitempty"`      // per rail-mapped function (spec.rails); bash: NUMVF_EW
	MTU         int    `json:"mtu,omitempty"`         // bash: MTU_EW, default 9216
	EswitchMode string `json:"eswitchMode,omitempty"` // switchdev | legacy (bash: ESWITCH_MODE)
	RoceCC      bool   `json:"roceCC,omitempty"`      // bash: ROCECC
	// PlanesNum reserves the multi-plane east-west topology (1|2|4, default
	// 1): each SuperNIC presents multiple ports, one per plane. Structure
	// only since 0.1.77 — no step consumes it yet; the mlxconfig firmware
	// keys land with the multi-plane work. The CRD schema carries the enum
	// and the default; the OrOrDefault accessors make the default real in
	// Go too (the Palette-enforced CRD copy can lag the repo).
	PlanesNum int `json:"planesNum,omitempty"`
	// NICBreakout reserves port breakout on ConnectX-8 and above (1|2|4,
	// default 1): a single physical port broken out into multiple PFs.
	// Same status as PlanesNum — structure only.
	NICBreakout int `json:"nicBreakout,omitempty"`
}

// PlanesNumOrDefault applies the schema default (1) to an absent field, so
// consumers never see the omitempty zero.
func (e EastWestSpec) PlanesNumOrDefault() int {
	if e.PlanesNum == 0 {
		return 1
	}
	return e.PlanesNum
}

// NICBreakoutOrDefault is PlanesNumOrDefault's breakout twin.
func (e EastWestSpec) NICBreakoutOrDefault() int {
	if e.NICBreakout == 0 {
		return 1
	}
	return e.NICBreakout
}

type NorthSouthSpec struct {
	LinkType      string `json:"linkType,omitempty"`      // bash: LINKTYPE_NS
	NumVFs        int    `json:"numVFs,omitempty"`        // per DPU function; bash: NUMVF_NS
	OffloadEngine string `json:"offloadEngine,omitempty"` // bash: DPUOFFLOAD
}

type Rail struct {
	Rail        string `json:"rail"`        // r0, r1, ...
	PCIFunction string `json:"pciFunction"` // "05:00" (bash: rails_pciaddr)
}

type HostBootSpec struct {
	IOMMU             string        `json:"iommu,omitempty"` // auto | intel | amd | off
	RDMANetnsMode     string        `json:"rdmaNetnsMode,omitempty"`
	Hugepages         HugepagesSpec `json:"hugepages,omitempty"`
	BootHook          *bool         `json:"bootHook,omitempty"`          // nil = on: render nodeprep-boot.service
	KubeletStateReset string        `json:"kubeletStateReset,omitempty"` // always | readyCheck | off
	MlnxInterfaceMgr  string        `json:"mlnxInterfaceMgr,omitempty"`  // wait | disable | ignore
}

// BootHookOn reports whether the nodeprep-boot.service oneshot should be
// rendered on the host (design §6.2). Unset means on — the hook replaces the
// bash script's rc.local entry and is the only OS boot integration
// nodeprep installs.
func (h HostBootSpec) BootHookOn() bool {
	return h.BootHook == nil || *h.BootHook
}

type HugepagesSpec struct {
	DefaultSize string `json:"defaultSize,omitempty"`
	Pages1G     int64  `json:"pages1G,omitempty"`
	Pages2M     int64  `json:"pages2M,omitempty"`
}

type PolicySpec struct {
	ControlDPU           bool   `json:"controlDPU,omitempty"`
	DisableACS           bool   `json:"disableACS,omitempty"`
	MaxConcurrentFlashes int    `json:"maxConcurrentFlashes,omitempty"` // fleet flash window (design §9.1)
	CAPause              bool   `json:"capiPause,omitempty"`            // pause CAPI Machines while prepping
	WorkerRoleLabel      string `json:"workerRoleLabel,omitempty"`      // manage | ignore
	LabelCompat          string `json:"labelCompat,omitempty"`          // v1: mirror legacy state label

	// TaintEnabled defaults to true: the nodeprep taint is applied at adoption
	// and released only after boot-verify (design §6.1). Pointer so the zero
	// value can be distinguished from an explicit false.
	TaintEnabled *bool `json:"taintEnabled,omitempty"`
	// RebootEnabled defaults to false; the agent also requires -allow-reboot.
	RebootEnabled *bool `json:"rebootEnabled,omitempty"`
	// HostMutations defaults to false: v0.1 steps are detect-only unless the
	// agent runs with -host-mutations (design: experimental gate).
	HostMutations *bool `json:"hostMutations,omitempty"`
}

func (p PolicySpec) TaintsOn() bool {
	return p.TaintEnabled == nil || *p.TaintEnabled
}
func (p PolicySpec) RebootsOn() bool {
	return p.RebootEnabled != nil && *p.RebootEnabled
}
func (p PolicySpec) MutationsOn() bool {
	return p.HostMutations != nil && *p.HostMutations
}

type SecretRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type DPUBMCSpec struct {
	CredentialsSecretRef *SecretRef `json:"credentialsSecretRef,omitempty"`
	UpdateBMC            bool       `json:"updateBMC,omitempty"`
	UpdateCEC            bool       `json:"updateCEC,omitempty"`
}

type NFSRDMASpec struct {
	Enabled bool `json:"enabled,omitempty"`
}

type ControlPlaneSpec struct {
	// Prep gates whether nodeprep runs on control-plane nodes at all
	// (design §3.1). Pointer with nil = prep them: the CP-prep posture is
	// the design default, and the zero value must not silently un-prep CPs
	// for profiles that never mention the block. This is the sole CP gate —
	// a former duplicate (policy.controlPlanePrep) shadowed it and kept a
	// prep=false CP in scope until 0.1.65 (found live on DSX Air).
	Prep           *bool  `json:"prep,omitempty"`
	ExpectedCount  int    `json:"expectedCount,omitempty"` // 0 = auto (KubeadmControlPlane replicas)
	Strategy       string `json:"strategy,omitempty"`      // serial | background
	BootstrapGate  string `json:"bootstrapGate,omitempty"` // auto | delay | off
	BootstrapDelay string `json:"bootstrapDelay,omitempty"`
}

// PrepOn reports whether control-plane nodes should be prepped; nil = true
// (see Prep).
func (c ControlPlaneSpec) PrepOn() bool {
	return c.Prep == nil || *c.Prep
}

type NodePrep struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodePrepSpec   `json:"spec"`
	Status            NodePrepStatus `json:"status,omitempty"`
}

type NodePrepSpec struct {
	NodeName   string     `json:"nodeName"`
	ProfileRef ProfileRef `json:"profileRef"`
}

type RebootStatus struct {
	Total    int            `json:"total,omitempty"`
	PerStage map[string]int `json:"perStage,omitempty"`
}

type StepStatus struct {
	Name        string       `json:"name"`
	Stage       Phase        `json:"stage"`
	State       StepState    `json:"state"`
	InputsHash  string       `json:"inputsHash,omitempty"`
	Attempts    int          `json:"attempts,omitempty"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	Message     string       `json:"message,omitempty"`
}

type NicStatus struct {
	PCI       string `json:"pci,omitempty"`     // 0000:05:00.0
	Fn        string `json:"fn,omitempty"`      // 05:00 (bash rail key form)
	Type      string `json:"type,omitempty"`    // SuperNIC | DPU | ConnectX-N | Mellanox | Unknown
	Variant   string `json:"variant,omitempty"` // Physical | Air
	Firmware  string `json:"firmware,omitempty"`
	PSID      string `json:"psid,omitempty"`
	Rail      string `json:"rail,omitempty"` // r0, dpu, r0_p0
	Rshim     string `json:"rshim,omitempty"`
	NetDev    string `json:"netdev,omitempty"`
	IBDev     string `json:"ibdev,omitempty"`
	LinkWidth string `json:"linkWidth,omitempty"`
	LinkSpeed string `json:"linkSpeed,omitempty"`
	DeviceID  string `json:"deviceID,omitempty"` // sysfs device id when MFT is absent
}

type GpuStatus struct {
	PCI       string `json:"pci,omitempty"`
	Name      string `json:"name,omitempty"`
	LinkWidth string `json:"linkWidth,omitempty"`
	LinkSpeed string `json:"linkSpeed,omitempty"`
}

type NodePrepStatus struct {
	Phase                     Phase              `json:"phase,omitempty"`
	ObservedProfileGeneration int64              `json:"observedProfileGeneration,omitempty"`
	Conditions                []metav1.Condition `json:"conditions,omitempty"`
	Reboots                   RebootStatus       `json:"reboots,omitempty"`
	BootID                    string             `json:"bootId,omitempty"`
	Steps                     []StepStatus       `json:"steps,omitempty"`
	Nics                      []NicStatus        `json:"nics,omitempty"`
	Gpus                      []GpuStatus        `json:"gpus,omitempty"`
}
