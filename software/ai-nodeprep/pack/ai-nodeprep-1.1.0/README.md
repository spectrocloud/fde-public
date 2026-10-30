# nodeprep

NodePrep prepares GPU worker nodes for AI fabrics. The pack deploys a cluster
controller and a privileged node agent. The controller adopts the nodes selected
by a `NodePrepProfile`, applies the taint that keeps workloads off a node while
it is being prepared, and tracks the node through provisioning, firmware
flashing, configuration and finalization. The agent performs the host work —
firmware and DOCA installation, adapter and DPU configuration, SR-IOV,
kernel-boot settings, OVS integration — and verifies the node after every boot
before the taint is released.

It is designed for NVIDIA-equipped clusters: BlueField-3 DPUs, ConnectX and
SuperNIC adapters, Spectrum-X fabrics, and hosts that need IOMMU, hugepages,
RDMA namespace mode and SR-IOV tuned consistently.


## Prerequisites

- Kubernetes 1.26 or newer.
- Worker nodes running a systemd-based Linux distribution that the agent can
  manage. The agent DaemonSet runs privileged with `hostPID` and mounts `/sys`,
  `/dev` and the host root; the namespace is labeled for privileged Pod
  Security enforcement.
- NVIDIA MFT installed on hosts that will have Mellanox adapters or DPUs
  configured (`mlxconfig`, `flint`). Firmware and DOCA artifacts must be
  reachable from the nodes at the URL configured in `firmware.source`.
- BlueField-3 DPUs with `rshim` available on hosts where DPU flashing is
  enabled (`northSouth.offloadEngine=true` and `policy.controlDPU=true`).
- Reboot permission for the agent when the profile may reboot nodes: the pack
  enables `agent.args.allowReboot` and `agent.args.hostMutations`, and the
  profile must also set `policy.rebootEnabled=true` and
  `policy.hostMutations=true` to take effect.
- If OVS is owned by an external manager such as the NVIDIA Spectrum-X
  controller, leave `eastWest.manageOVS=false` (the default). NodePrep still
  applies the five switchdev `other_config` parameters the datapath requires,
  but does not manage OVS services, the database, bridges or PF attachment.


## Parameters

The pack deploys the NodePrep chart with the values below. The most commonly
changed settings are listed first; the complete `NodePrepProfile` reference
follows.

### Chart values

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `controller.image.repository` | Controller image repository. | String | `ghcr.io/spectrocloud/fde-public/ai-nodeprep` | No |
| `controller.image.tag` | Controller image tag. Empty uses `<appVersion>-controller`. | String | `""` (derived) | No |
| `agent.image.repository` | Agent image repository. | String | `ghcr.io/spectrocloud/fde-public/ai-nodeprep` | No |
| `agent.image.tag` | Agent image tag. Empty uses `<appVersion>-agent`. | String | `""` (derived) | No |
| `agent.args.interval` | How often the agent polls its NodePrep and walks steps. | String (duration) | `5s` | No |
| `agent.args.hostMutations` | Allows steps to change the host. When false the walk is detect-only and mutating steps report Blocked. | Boolean | `true` in the pack (`false` in the chart) | No |
| `agent.args.allowReboot` | Allows the agent to reboot hosts when a step requires it. | Boolean | `true` in the pack (`false` in the chart) | No |
| `agent.args.verbose` | Logs every host command. Can also be toggled with the `NODEPREP_VERBOSE` environment variable without a rollout. | Boolean | `false` | No |
| `agent.args.rebootCommand` | Command used to reboot the host. Empty uses `nsenter -t 1 -m -u -i -n -- systemctl reboot`. | String | `""` | No |
| `namespace.create` | Creates the `nodeprep-system` namespace with privileged Pod Security labels. Set false when Palette or another system manages the namespace. | Boolean | `false` in the pack (`true` in the chart) | No |
| `crds.create` | Renders the `NodePrepProfile` and `NodePrep` CRDs as chart templates so `helm upgrade` applies schema changes. Set false when an external system owns the CRDs. | Boolean | `true` | No |
| `controller.resources` / `agent.resources` | Resource requests and limits for the controller and agent. | Object | See `values.yaml` | No |

### `selection` — which nodes are adopted

A profile without a `selection` block adopts no nodes.

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `selection.mode` | How nodes are chosen. `labelSelector` uses `nodeSelector`; `allWorkers` takes every non-control-plane node; `allNodes` also includes control planes (subject to `controlPlane.prep`). | String (`labelSelector`, `allWorkers`, `allNodes`) | `labelSelector` | No |
| `selection.nodeSelector` | Label gate for `labelSelector` mode. Supports `matchLabels` and `matchExpressions` with `In`, `NotIn`, `Exists` and `DoesNotExist`. | LabelSelector | Empty (matches every node) | No |
| `selection.excludeLabel` | Excludes nodes in every mode. `key` excludes any value; `key=value` excludes that exact value. | String | None | No |
| `selection.waitForFipsKernel` | Holds every NodePrep action until the node runs a FIPS kernel, detected from `uname -r` containing `fips`. Use it when the node boots with a non-FIPS kernel and a cluster-profile action installs the FIPS kernel afterwards, so DOCA and the kernel installation never run in parallel. | Boolean | `false` | No |

### `firmware` — BFB and DOCA artifacts

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `firmware.source` | Base URL from which BFB and DOCA artifacts are downloaded. | String (URL) | None — download steps skip | No |
| `firmware.aptUpgrade` | Runs `apt-get update` and `apt-get upgrade` before package installation. | Boolean | `false` | No |
| `firmware.bfb.name` | BFB firmware bundle filename, relative to `firmware.source`. | String | None — no BFB step | No |
| `firmware.bfb.minVersion` | Firmware floor for the BFB flash gate on BlueField-3. A card below it is flashed; a card at or above it needs no flash. Dot-separated numeric comparison with zero-padding (`32.49` < `32.49.1014`). | String | None (no version gate) | No |
| `firmware.bfb.sha256` | SHA-256 of the BFB. A cached copy that still matches is reused. | String | None (no verification) | No |
| `firmware.doca.deb` | DOCA host package filename, relative to `firmware.source`. | String | None — no DOCA install | No |
| `firmware.doca.sha256` | SHA-256 of the DOCA package. | String | None (no verification) | No |
| `firmware.doca.packages` | Packages installed with the DOCA package. The placeholder `$(uname -r)` is expanded to the host kernel release. | List of strings | Empty | No |

### `eastWest` — GPU fabric rail

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `eastWest.linkType` | Link type configured on rail adapters. | String (`InfiniBand`, `Ethernet`) | Unset — left unmanaged | No |
| `eastWest.numVFs` | SR-IOV VFs per rail-mapped adapter. | Integer or numeric string | `0` | No |
| `eastWest.mtu` | Rail MTU. In switchdev mode bridges, VFs and representors use this value minus 50 bytes to leave headroom. `9216` is recommended for a configured GPU fabric. | Integer or numeric string | Unset — MTU left unmanaged | No |
| `eastWest.eswitchMode` | Eswitch mode applied through devlink. Only `switchdev` enables the switchdev-specific work (VFs, OVS `other_config`, bridges). | String (`switchdev`, `legacy`) | Unset — left unmanaged | No |
| `eastWest.manageOVS` | OVS ownership. `false` leaves services, database, bridges, PF attachment and ownership records to an external manager while NodePrep applies the five switchdev `other_config` parameters; `true` manages the complete DOCA OVS setup. | Boolean | `false` | No |
| `eastWest.roceCC` | Applies RoCE congestion-control and adaptive-routing keys on SuperNIC and ConnectX-7/8/9 adapters. | Boolean | `false` | No |
| `eastWest.planesNum` | Multi-plane east-west topology, one port per plane per SuperNIC. Reserved: accepted by the schema but not applied yet. | Integer or numeric string (`1`, `2`, `4`) | `1` | No |
| `eastWest.nicBreakout` | Port breakout on ConnectX-8 and newer. Reserved: accepted by the schema but not applied yet. | Integer or numeric string (`1`, `2`, `4`) | `1` | No |

### `northSouth` — DPUs

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `northSouth.linkType` | Link type configured on DPU functions. | String (`InfiniBand`, `Ethernet`) | Unset — left unmanaged | No |
| `northSouth.numVFs` | VFs per DPU function when DPU management is enabled. | Integer or numeric string | `0` | No |
| `northSouth.offloadEngine` | Enables DPU management. `true` flashes and configures BlueField DPUs; `false` leaves DPUs untouched. | Boolean | `false` | No |

### `rails` — fabric adapters

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `rails[].rail` | Rail name, for example `r0` or `r1_p0`. | String matching `r<N>` or `r<N>_p<N>` | None | Yes, per entry |
| `rails[].pciFunction` | PCI function of the adapter without the domain, for example `05:00`. | String | None | Yes, per entry |

Only adapters listed here are configured.

### `hostBoot` — host boot settings

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `hostBoot.iommu` | IOMMU kernel setting staged for the host. | String (`auto`, `intel`, `amd`, `off`) | Unset — left unmanaged | No |
| `hostBoot.rdmaNetnsMode` | `ib_core` network-namespace mode. `exclusive` maps to netns_mode 0; `shared` maps to 1. Legacy `"0"` and `"1"` values in stored profiles are still read. | String (`exclusive`, `shared`) | Unset — left unmanaged | No |
| `hostBoot.hugepages.defaultSize` | Default hugepage size. | String (`2M`, `1G`) | Unset — left unmanaged | No |
| `hostBoot.hugepages.pages1G` | Number of 1 GiB hugepages. | Integer or numeric string | `0` | No |
| `hostBoot.hugepages.pages2M` | Number of 2 MiB hugepages. | Integer or numeric string | `0` | No |
| `hostBoot.bootHook` | Renders the `nodeprep-boot.service` oneshot that carries the kubelet manager-state reset and the Mellanox interface-manager wait into every boot. | Boolean | `true` | No |
| `hostBoot.kubeletStateReset` | Guarded kubelet CPU/Memory-manager state reset. `always` runs at prep and every boot; `readyCheck` runs only while the kubelet is not running; `off` disables it. | String (`always`, `readyCheck`, `off`) | `off` | No |
| `hostBoot.mlnxInterfaceMgr` | How the boot hook handles the Mellanox interface manager: wait for it, disable it, or ignore it. | String (`wait`, `disable`, `ignore`) | `ignore` | No |

### `policy` — safety and fleet control

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `policy.controlDPU` | Allows DPU control. Required in addition to `northSouth.offloadEngine=true`. | Boolean | `false` | No |
| `policy.disableACS` | Disables PCIe ACS on adapters, which peer-to-peer GPU traffic requires. | Boolean | `false` | No |
| `policy.maxConcurrentFlashes` | Fleet-wide flash window: how many nodes may flash at once. `0` means unlimited. | Integer or numeric string | `0` | No |
| `policy.capiPause` | Pauses the CAPI Machine while the node is prepared. | Boolean | `false` | No |
| `policy.workerRoleLabel` | Adds and restores the Kubernetes worker role label when the node is Ready; `ignore` leaves it untouched. | String (`manage`, `ignore`) | `manage` | No |
| `policy.labelCompat` | Mirrors the legacy `spectrocloud.com/nodeprep` label during migration. `false` removes it when present. | Boolean | `false` | No |
| `policy.taintEnabled` | Applies the `spectrocloud.com/nodeprep` taint at adoption and releases it only after boot verification. Set `false` for a non-disruptive run. | Boolean | `true` | No |
| `policy.rebootEnabled` | Allows the profile to request reboots. The agent also requires `agent.args.allowReboot`. | Boolean | `false` | No |
| `policy.hostMutations` | Allows steps to mutate the host. The agent also requires `agent.args.hostMutations`. | Boolean | `false` | No |

### `dpuBMC` — DPU BMC and CEC firmware

Reserved: accepted by the schema but not applied yet.

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `dpuBMC.credentialsSecretRef.name` | Secret containing DPU BMC credentials. | String | None | Yes, when the block is used |
| `dpuBMC.credentialsSecretRef.namespace` | Namespace of the credentials Secret. | String | Secret's own namespace | No |
| `dpuBMC.updateBMC` | Updates DPU BMC firmware. | Boolean | `false` | No |
| `dpuBMC.updateCEC` | Updates DPU CEC firmware. | Boolean | `false` | No |

### `nfsRdma`

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `nfsRdma.enabled` | Configures NFS over RDMA on the node. | Boolean | `false` | No |

### `controlPlane` — control-plane choreography

| **Parameter** | **Description** | **Type** | **Default Value** | **Required** |
|---|---|---|---|---|
| `controlPlane.prep` | Allows control-plane nodes to be prepared. This is the only control-plane gate; `selection` must also include them. | Boolean | `true` | No |
| `controlPlane.expectedCount` | Expected control-plane size, used to compute the maintenance window. `0` derives the value from the KubeadmControlPlane replicas. | Integer or numeric string | `0` (auto) | No |
| `controlPlane.strategy` | `serial` admits one control-plane node past provisioning at a time; `background` applies no admission limit. | String (`serial`, `background`) | `serial` | No |
| `controlPlane.bootstrapGate` | Reserved: accepted by the schema but not applied yet. | String (`auto`, `delay`, `off`) | Unset | No |
| `controlPlane.bootstrapDelay` | Reserved: accepted by the schema but not applied yet. | String (duration) | Unset | No |


## Upgrade

- **Upgrading to 1.1.0** adds `selection.waitForFipsKernel`. The CRDs are chart templates, so a Helm or Palette upgrade applies the new schema automatically; no manual CRD step is required. Upgrades preserve existing `NodePrep` and `NodePrepProfile` objects and their progress ledgers.
  - Retry behavior also changed: a step now gets 10 counted attempts with a longer exponential backoff, and failures caused by an unavailable Kubernetes API server or a DNS server temporarily failing to respond no longer consume the retry budget.
  - A DNS name that does not resolve (`no such host`, `name or service not known`) is treated as a configuration error and still consumes the budget.
- **If `crds.create=false`** because an external system owns the CRDs, that system must apply the updated schema before deploying this version.
- **The pack enables host mutations and reboots** (`agent.args.hostMutations` and `agent.args.allowReboot`). Confirm that the profile's `policy.hostMutations` and `policy.rebootEnabled` match the intended posture.
- **From 1.0.0 onward the images are published at** `ghcr.io/spectrocloud/fde-public/ai-nodeprep`. Cluster profiles that pinned an earlier image registry must update the image repositories.


## Usage

1. Create an add-on cluster profile and add the **nodeprep** pack to it.
2. Deploy the profile to a cluster. The pack creates the controller Deployment and the agent DaemonSet in `nodeprep-system`. The agent runs on every node, including control planes, because the taint it manages can land on any node.
3. Create a cluster-scoped `NodePrepProfile` that selects the nodes to prepare. In Palette, add it as a manifest layer to the profile; outside Palette, apply it with `kubectl`.

```yaml
apiVersion: nodeprep.spectrocloud.com/v1alpha1
kind: NodePrepProfile
metadata:
  name: gpu-worker
spec:
  selection:
    mode: labelSelector
    nodeSelector:
      matchLabels:
        node.spectrocloud.com/ai-worker: "true"
  rails:
    - rail: r0
      pciFunction: "05:00"
    - rail: r1
      pciFunction: "06:00"
  eastWest:
    linkType: Ethernet
    numVFs: 1
    mtu: 9216
    eswitchMode: switchdev
    manageOVS: false
  policy:
    hostMutations: true
    rebootEnabled: true
    taintEnabled: true
```

4. As soon as a node is adopted, the controller applies the `spectrocloud.com/nodeprep` taint so workloads are not scheduled onto a node that is being prepared. The agent walks the node through the stages and verifies it after every boot; the taint is released only when verification passes.
5. For FIPS environments where the node first boots with a non-FIPS kernel and the FIPS kernel is installed later by a cluster-profile action, set `selection.waitForFipsKernel: true`. The agent performs no NodePrep action until it detects a FIPS kernel, then proceeds automatically.

> [!CAUTION]
> The agent is privileged and mounts the host filesystem, `/sys` and `/dev`, and it can reboot nodes. Deploy it only on clusters where that level of host access is intended, and validate the profile on a non-production cluster first.


## References

- [NodePrep controller source](https://github.com/spectrocloud/fde-public/tree/nodeprep-controller/software/ai-nodeprep/controller)
- [NVIDIA DOCA documentation](https://docs.nvidia.com/doca/)
- [NVIDIA MFT documentation](https://docs.nvidia.com/networking/display/mftv/)
- [Kubernetes device plugins](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/)
- [Spectro Cloud pack documentation](https://docs.spectrocloud.com/registries-and-packs/)
