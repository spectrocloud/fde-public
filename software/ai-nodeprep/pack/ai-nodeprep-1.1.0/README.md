# nodeprep Helm chart

Packages the NodePrep solution for Helm-based deployment: the cluster
controller (Deployment), the privileged node agent (DaemonSet), the RBAC
contract, and the NodePrepProfile/NodePrep CRDs.

This mirrors `manifests/*.yaml` 1:1 — that directory stays the
externally-enforced source of truth (Palette); the chart is the packaged
form for Helm-consuming deployers. When behavior changes, the manifests
change first and the chart follows.

## Install

```sh
# Standalone (chart creates the namespace and CRDs):
helm upgrade --install nodeprep chart/nodeprep \
  --set agent.args.hostMutations=true \
  --set agent.args.allowReboot=true

# Namespace and/or CRDs managed externally (e.g. Palette enforces both):
helm upgrade --install nodeprep chart/nodeprep \
  --set namespace.create=false \
  --set agent.args.hostMutations=true \
  --set agent.args.allowReboot=true

# CRDs managed by an external system only — skip the chart's copies:
helm upgrade --install nodeprep chart/nodeprep \
  --set crds.create=false \
  --set agent.args.hostMutations=true \
  --set agent.args.allowReboot=true
```

## CRDs: templates, not a crds/ directory

The two CRDs live in `templates/` (wrapped in `{{- if .Values.crds.create }}`,
default on) rather than the conventional `crds/` subdirectory. That is a
deliberate choice: `crds/` contents are installed once at `helm install` and
**never upgraded** by `helm upgrade` — Helm's documented limitation — so a
schema evolution would silently keep the old CRD forever. As templates they
ride every upgrade. The trade-off, accepted knowingly: `helm uninstall` deletes
the CRDs (removing all NodePrep/NodePrepProfile objects and their ledgers), and
Helm "owns" objects other tools may also manage. On clusters where an external
system (e.g. Palette) enforces the CRDs, set `crds.create=false` to avoid two
owners fighting.

Manual `kubectl apply` of the raw CRD manifests on a chart-managed cluster
re-stamps Helm's ownership markers afterwards (`make crd-apply` does it) —
without `meta.helm.sh/release-*` and `app.kubernetes.io/managed-by: Helm` the
next `helm upgrade` that must create or adopt the CRD fails the ownership
validation.

## What it deploys

| Template | Kind | Notes |
|---|---|---|
| `namespace.yaml` | Namespace | PSA `privileged` (agent needs it). Kept on uninstall. Skippable via `namespace.create=false`. |
| `crd-nodeprep*.yaml` | 2 CRDs | Rendered templates (`crds.create=false` to skip); upgraded on every `helm upgrade` — see the section above for the trade-offs. |
| `serviceaccounts.yaml` | 2 SAs | controller + agent. |
| `clusterroles.yaml` | 2 ClusterRoles + 2 bindings | Mirrors `manifests/rbac.yaml` (the agent's `pods list/delete` backs the 0.1.62 device-plugin bounce). |
| `controller-deployment.yaml` | Deployment | Hardened (non-root, read-only fs, caps dropped), tolerates the nodeprep + control-plane taints. |
| `agent-daemonset.yaml` | DaemonSet | `hostPID`, privileged, `/sys` + `/dev` + `/` host mounts, tolerates the nodeprep + control-plane taints (load-bearing: the taint survives reboots and only the agent releases it). |

## Values that matter

| Value | Default | Meaning |
|---|---|---|
| `controller.image.tag` | `<appVersion>-controller` | Pin to override (registry mirror). |
| `agent.image.tag` | `<appVersion>-agent` | Same. One tag per version — tags are never re-pushed. |
| `agent.args.hostMutations` | `false` | Detect-only posture when false: mutating steps report Blocked instead of touching the host. |
| `agent.args.allowReboot` | `false` | The agent also requires this to reboot the host. |
| `agent.args.interval` | `5s` | Walk poll cadence. |
| `agent.args.verbose` / `NODEPREP_VERBOSE` env | off | Full host-exec trace for troubleshooting; the env toggle works without a rollout. |
| `agent.args.rebootCommand` | binary default | `nsenter -t 1 -m -u -i -n -- systemctl reboot`. |
| `namespace.create` | `true` | false when the namespace (and its PSA labels) is external. |

## Versioning

`Chart.yaml` `version` and `appVersion` both track the release (e.g. `0.1.81`).
`appVersion` drives both default image tags (`<appVersion>-agent` /
`-controller`); `version` names the packaged tarball
(`nodeprep-<version>.tgz`), which the Palette pack's `charts` array points at.
Bump both together with the manifests `VERSION` bump.

## Sync helper

The CRD templates must stay byte-identical to `manifests/crd-*.yaml`:

```sh
make chart-sync-crds
```

## NodePrepProfile reference

A `NodePrepProfile` is cluster-scoped (`nodeprep.spectrocloud.com/v1alpha1`). It
selects nodes and declares how to prep them; every setting lives under `spec`.
A field is **optional** unless the table says required, and "default" describes
the behavior when it is omitted. Integer fields marked *(int/string)* also
accept a numeric string (e.g. `"8"`), because Palette templating delivers
values as strings. The `status` section is written by the controller/agent, not
by the operator.

### `selection` — which nodes are adopted

A profile with no `selection` block adopts **no nodes** (this is the one
effectively required section).

| Setting | What it is for | Allowed values | Default |
|---|---|---|---|
| `mode` | How the node set is chosen. `allWorkers` skips control-plane nodes; `allNodes` includes them (subject to `controlPlane.prep`). | `labelSelector`, `allWorkers`, `allNodes` | `labelSelector` |
| `nodeSelector` | Label gate used with `labelSelector` mode; standard `matchLabels` and `matchExpressions` (`In`, `NotIn`, `Exists`, `DoesNotExist`). | LabelSelector object | empty — matches every node |
| `excludeLabel` | Disqualifies nodes in every mode. `key` excludes any value; `key=value` excludes that exact one. | string | none |
| `waitForFipsKernel` | Holds every nodeprep action (inventory, boot hook, walk, boot-verify) until the node runs a FIPS kernel — `uname -r` contains `fips`. For FIPS environments where the node boots non-FIPS and a cluster-profile action installs the FIPS kernel afterwards, so DOCA and the kernel install never race. Applied per cycle; a reboot into the FIPS kernel starts a fresh agent that proceeds. | boolean | `false` |

### `firmware` — BFB and DOCA artifacts

| Setting | What it is for | Allowed values | Default |
|---|---|---|---|
| `source` | Base URL for BFB/DOCA downloads (e.g. a MAAS/TFTP mirror). | URL string | unset — download steps skip |
| `aptUpgrade` | Run `apt-get update && apt-get upgrade` before package installation. | boolean | `false` |
| `bfb.name` | BFB firmware bundle filename, relative to `source`. | string | unset — no BFB step |
| `bfb.minVersion` | Firmware floor for the BFB flash gate (BlueField-3 only). A card whose running firmware sorts below it is flashed; one at or above needs no flash. Dot-separated numeric comparison with zero-padding (`32.49` < `32.49.1014`). ConnectX-class adapters are never compared. | numeric dotted string | unset — no version gate |
| `bfb.sha256` | SHA-256 of the BFB; a cached copy that still matches is reused. | 64-char hex string | unset — no checksum verification |
| `doca.deb` | DOCA host package filename, relative to `source`. | string | unset — no DOCA install |
| `doca.sha256` | SHA-256 of the DOCA deb. | 64-char hex string | unset — no checksum verification |
| `doca.packages` | Packages installed in the deb's apt transaction. Only the placeholder `$(uname -r)` is expanded (to the host kernel release); any other shell expression fails the step. | list of package names | empty |

### `eastWest` — the GPU fabric rail

| Setting | What it is for | Allowed values | Default |
|---|---|---|---|
| `linkType` | Link type applied to the rail adapters. | `InfiniBand`, `Ethernet` | unset — link type left unmanaged |
| `numVFs` | SR-IOV VFs per rail-mapped PF. | *(int/string)* ≥ 0 | `0` |
| `mtu` | Rail MTU. In switchdev the bridges/VFs/representors get `mtu - 50` (bash headroom). | *(int/string)* > 0 | unset/`0` — MTU left unmanaged (the bash default for a configured fabric is `9216`) |
| `eswitchMode` | Eswitch mode applied through devlink. Only `switchdev` engages the switchdev-specific work (VFs, OVS other_config, bridges). | `switchdev`, `legacy` | unset — eswitch mode left unmanaged |
| `manageOVS` | OVS ownership gate. `false` leaves services, conf.db, bridges, PF attachment and the ownership record to an external manager (e.g. Spectrum-X controller) while nodeprep still applies the five switchdev `other_config` parameters; `true` is full DOCA OVS management. | boolean | `false` |
| `roceCC` | Applies the RoCE congestion-control / adaptive-routing keys (`ROCE_ADAPTIVE_ROUTING_EN`, `USER_PROGRAMMABLE_CC`, …) on SuperNIC and ConnectX-7/8/9 adapters. | boolean | `false` |
| `planesNum` | Multi-plane east-west topology (one port per plane per SuperNIC). **Reserved:** declared in the schema, not yet applied by any step. | *(int/string)* `1`, `2`, `4` | `1` |
| `nicBreakout` | Port breakout on ConnectX-8 and above (one physical port into multiple PFs). **Reserved:** declared, not yet applied. | *(int/string)* `1`, `2`, `4` | `1` |

### `northSouth` — DPUs

| Setting | What it is for | Allowed values | Default |
|---|---|---|---|
| `linkType` | Link type for the DPU functions. | `InfiniBand`, `Ethernet` | unset — left unmanaged |
| `numVFs` | VFs per DPU function when DPU management is enabled. | *(int/string)* ≥ 0 | `0` |
| `offloadEngine` | Master DPU gate: `true` flashes and configures BlueField DPUs (BFB, mlxconfig, VFs); `false` leaves DPUs entirely alone. | boolean | `false` |

### `rails` — which adapters form the fabric

| Setting | What it is for | Allowed values | Default |
|---|---|---|---|
| `rails[].rail` | Rail name. | string matching `r<N>` or `r<N>_p<N>` (e.g. `r0`, `r1_p0`) | **required** per entry |
| `rails[].pciFunction` | PCI function of the adapter (`DDDD:BB:DD.F` without the domain, e.g. `05:00`). | string | **required** per entry |

The list itself is optional; only rail-mapped devices are configured.

### `hostBoot` — OS-level boot behavior

| Setting | What it is for | Allowed values | Default |
|---|---|---|---|
| `iommu` | IOMMU kernel setting staged for the host. | `auto`, `intel`, `amd`, `off` | unset — left unmanaged |
| `rdmaNetnsMode` | `ib_core` netns mode: `exclusive` (netns_mode=0) or `shared` (=1). Legacy `"0"`/`"1"` values in stored profiles are still read. | `exclusive`, `shared` | unset — left unmanaged |
| `hugepages.defaultSize` | Default hugepage size. | string, e.g. `2M`, `1G` | unset — left unmanaged |
| `hugepages.pages1G` | Number of 1 GiB hugepages. | *(int/string)* ≥ 0 | `0` |
| `hugepages.pages2M` | Number of 2 MiB hugepages. | *(int/string)* ≥ 0 | `0` |
| `bootHook` | Render the `nodeprep-boot.service` oneshot that carries the kubelet manager-state reset and the `mlnx_interface_mgr` wait into every boot. | boolean | unset = `true` |
| `kubeletStateReset` | Guarded kubelet CPU/Memory-manager state reset: `always` (at prep and every boot), `readyCheck` (only when the kubelet is not running), `off`. | `always`, `readyCheck`, `off` | unset = `off` |
| `mlnxInterfaceMgr` | Handling of the Mellanox interface manager in the boot hook: `wait` for it, `disable` it, or `ignore` it. | `wait`, `disable`, `ignore` | unset = `ignore` |

### `policy` — safety and fleet control

| Setting | What it is for | Allowed values | Default |
|---|---|---|---|
| `controlDPU` | Allows DPU control at all (required in addition to `northSouth.offloadEngine=true`). | boolean | `false` |
| `disableACS` | Disables PCIe ACS on the adapters (required for peer-to-peer GPU traffic). | boolean | `false` |
| `maxConcurrentFlashes` | Fleet-wide flash window: how many nodes may flash at once. `0` = unlimited. | *(int/string)* ≥ 0 | `0` |
| `capiPause` | Pauses the CAPI Machine while the node is being prepped. | boolean | `false` |
| `workerRoleLabel` | Adds/restores the Kubernetes worker role label when the node is Ready; `ignore` leaves it untouched. | `manage`, `ignore` | `manage` |
| `labelCompat` | Legacy migration shim: mirrors the old `spectrocloud.com/nodeprep` label. `false` removes it when present. | boolean | `false` |
| `taintEnabled` | Applies the `spectrocloud.com/nodeprep` taint at adoption, releasing it only after boot-verify. Set `false` for a non-disruptive/observe-only run. | boolean | `true` |
| `rebootEnabled` | Allows the profile to request reboots (the agent also requires `-allow-reboot`). | boolean | `false` |
| `hostMutations` | Allows any step that mutates the host (the agent also requires `-host-mutations`). With it false the walk is detect-only and mutating steps report Blocked. | boolean | `false` |

### `dpuBMC` — DPU BMC/CEC flashing

**Reserved:** the schema declares these settings, but no step consumes them yet.

| Setting | What it is for | Allowed values | Default |
|---|---|---|---|
| `credentialsSecretRef.name` | Secret containing the DPU BMC credentials. | string | **required** when this block is used |
| `credentialsSecretRef.namespace` | Namespace of that Secret. | string | secret's own default |
| `updateBMC` | Update the DPU BMC firmware. | boolean | `false` |
| `updateCEC` | Update the DPU CEC firmware. | boolean | `false` |

### `nfsRdma`

| Setting | What it is for | Allowed values | Default |
|---|---|---|---|
| `enabled` | Configures NFS over RDMA on the node. | boolean | `false` |

### `controlPlane` — control-plane choreography

| Setting | What it is for | Allowed values | Default |
|---|---|---|---|
| `prep` | Whether control-plane nodes may be prepped at all. Omitted means they may; this is the sole control-plane gate (selection still has to include them). | boolean | unset = `true` |
| `expectedCount` | Expected control-plane size; used to compute the maintenance window (expected − quorum). `0` derives it from the KubeadmControlPlane replicas. | *(int/string)* ≥ 0 | `0` (auto) |
| `strategy` | `serial` admits one control-plane node past Provisioning at a time; `background` applies no admission limit. | `serial`, `background` | `serial` |
| `bootstrapGate` | **Reserved:** declared in the schema, not yet applied. | `auto`, `delay`, `off` | unset |
| `bootstrapDelay` | **Reserved:** declared in the schema, not yet applied. | duration string, e.g. `5m` | unset |

## Example NodePrepProfile

In order to trigger nodeprep on nodes, deploy a NodePrepProfile resource:

```
apiVersion: nodeprep.spectrocloud.com/v1alpha1
kind: NodePrepProfile
metadata:
  name: basic-profile
spec:
  selection:
    mode: allNodes
    # mode: labelSelector
    # nodeSelector:
    #   matchLabels:
    #     node.spectrocloud.com/ai-worker: "true"
    excludeLabel: node.spectrocloud.com/ai-worker=false
    # waitForFipsKernel: true   # 1.1.0: hold all actions until a FIPS kernel boots

  controlPlane:
    bootstrapGate: auto
    expectedCount: 0
    prep: false
    strategy: serial
  eastWest:
    linkType: "Ethernet"
    numVFs: 0
    mtu: 9000
    eswitchMode: legacy
    manageOVS: false
    roceCC: true
  firmware:
    aptUpgrade: true
    bfb:
      name: bf-fwbundle-3.5.0-89_26.07-prod.bfb
      sha256: 2d02f198b952b3e6a652beccbd7740ed4ab78ded3d00c3939acd1d707c93391d
    doca:
      deb: doca-host_3.5.0-082000-26.07-ubuntu2404_amd64.deb
      sha256: a2312bb04b980fae370fb5de483fda8cc6bc590553bf3c8dc77f980e3840740a
      packages:
      - linux-headers-$(uname -r)
      - gcc-12
      - libgcc-12-dev
      - doca-all
      - lldpd
      - mft
      - netplan.io
      - pv
      - psmisc
    source: "http://maas.internal:8069/rcp" # base URL for artifacts (MAAS mirror)
  hostBoot:
    kubeletStateReset: always
    mlnxInterfaceMgr: wait
    rdmaNetnsMode: exclusive # or shared
    iommu: auto
    hugepages:
      defaultSize: 2M
      pages1G: 0
      pages2M: 0
  nfsRdma:
    enabled: true
  northSouth:
    linkType: "Ethernet"
    numVFs: 0
    offloadEngine: false
  policy:
    capiPause: true
    controlDPU: false
    disableACS: true
    hostMutations: true
    labelCompat: false
    maxConcurrentFlashes: 1
    rebootEnabled: true
    taintEnabled: true
    workerRoleLabel: manage
  rails: []
    # - rail: r0
    #   pciFunction: "05:00"
    # - rail: r1
    #   pciFunction: "06:00"
    # - rail: r2
    #   pciFunction: "07:00"
    # - rail: r3
    #   pciFunction: "08:00"
    # - rail: r4
    #   pciFunction: "09:00"
    # - rail: r5
    #   pciFunction: "0a:00"
    # - rail: r6
    #   pciFunction: "0b:00"
    # - rail: r7
    #   pciFunction: "0c:00"
```