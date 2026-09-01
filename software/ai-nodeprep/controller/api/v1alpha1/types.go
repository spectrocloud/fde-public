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
	// NodeSelector decides which Nodes are adopted.
	NodeSelector *metav1.LabelSelector `json:"nodeSelector"`

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

type FirmwareSource struct {
	Source     string     `json:"source,omitempty"` // e.g. http://maas.internal:8069/rcp
	BFB        BFBSource  `json:"bfb,omitempty"`
	DOCA       DOCASource `json:"doca,omitempty"`
	AptUpgrade bool       `json:"aptUpgrade,omitempty"`
}

type BFBSource struct {
	Name       string `json:"name,omitempty"`
	MinVersion string `json:"minVersion,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}

type DOCASource struct {
	Deb      string   `json:"deb,omitempty"`
	SHA256   string   `json:"sha256,omitempty"`
	Packages []string `json:"packages,omitempty"`
}

type EastWestSpec struct {
	LinkType    string `json:"linkType,omitempty"`    // InfiniBand | Ethernet (bash: LINKTYPE_EW 1|2)
	NumVFs      int    `json:"numVFs,omitempty"`      // bash: NUMVF_EW
	MTU         int    `json:"mtu,omitempty"`         // bash: MTU_EW, default 9216
	EswitchMode string `json:"eswitchMode,omitempty"` // switchdev | legacy (bash: ESWITCH_MODE)
	RoceCC      bool   `json:"roceCC,omitempty"`      // bash: ROCECC
}

type NorthSouthSpec struct {
	LinkType      string `json:"linkType,omitempty"`      // bash: LINKTYPE_NS
	NumVFs        int    `json:"numVFs,omitempty"`        // bash: NUMVF_NS
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
	BootHook          bool          `json:"bootHook,omitempty"`
	KubeletStateReset string        `json:"kubeletStateReset,omitempty"` // always | readyCheck | off
	MlnxInterfaceMgr  string        `json:"mlnxInterfaceMgr,omitempty"`  // wait | disable | ignore
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
	CordonDuringVFConfig bool   `json:"cordonDuringVFConfig,omitempty"`
	CAPause              bool   `json:"capiPause,omitempty"`        // pause CAPI Machines while prepping
	WorkerRoleLabel      string `json:"workerRoleLabel,omitempty"`  // manage | ignore
	LabelCompat          string `json:"labelCompat,omitempty"`      // v1: mirror legacy state label
	ControlPlanePrep     bool   `json:"controlPlanePrep,omitempty"` // may prep control-plane nodes

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
	Prep           bool   `json:"prep,omitempty"`
	ExpectedCount  int    `json:"expectedCount,omitempty"` // 0 = auto (KubeadmControlPlane replicas)
	Strategy       string `json:"strategy,omitempty"`      // serial | background
	BootstrapGate  string `json:"bootstrapGate,omitempty"` // auto | delay | off
	BootstrapDelay string `json:"bootstrapDelay,omitempty"`
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
