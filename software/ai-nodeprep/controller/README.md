# nodeprep-controller

Go implementation of [NP-CTRL-001](../design/nodeprep-controller-design.html):
a Kubernetes controller + node agent that replaces both the 979-line bash
`nodeprep-v105.sh` and the legacy `software/nodeprep-controller` Deployment.

## Architecture

Two small components, one CRD pair:

| Component | Runs as | Owns |
|---|---|---|
| **controller** | Deployment, `nodeprep-system` | Adoption (Node → NodePrep), taint contract, legacy-label mirror, worker-role label, CAPI Machine pause, flash-window & control-plane admission, profile watching |
| **agent** | privileged DaemonSet, `hostPID` | The NodePrep phase machine on its node: step ledger, hardware inventory, boot_id protocol, boot-verify |

`NodePrepProfile` (cluster-scoped) is the declarative spec — the knobs
`nodeprep-v105.sh` took as variables. `NodePrep` (cluster-scoped, node-named)
is the per-node state machine; its status is the ledger. All progress lives in
the NodePrep object, so both components are stateless across restarts.

Phases map 1:1 onto the bash label values:
`init→Provisioning → inithw→Flashing → config→Configuring → precomplete/complete→Finalizing → Ready`.

## v0.1 scope

Real, exercised end-to-end:

- **Adoption**: nodes matching a profile's selection get a NodePrep; the
  legacy `spectrocloud.com/nodeprep` label is imported and mirrored
  (`policy.labelCompat: v1`).
- **Node selection** (§3.1): `spec.selection` picks the node set —
  `mode: labelSelector` (default) gates on `selection.nodeSelector`;
  `mode: allWorkers` adopts every non-control-plane node, no label needed;
  `mode: allNodes` also adopts control planes (with
  `policy.controlPlanePrep: true`; quorum choreography below).
  `selection.excludeLabel ("key" or "key=value") disqualifies individual
  nodes under every mode.
- **Control-plane prep** (§6.4): with `policy.controlPlanePrep: true`,
  control-plane nodes run the same step machine, admitted through the quorum
  window — at most `expected − quorum(expected)` members mid-prep
  concurrently (1 of 3, 2 of 5), computed from the KubeadmControlPlane
  replicas (`controlPlane.expectedCount` overrides). While one member is
  mid-prep the others hold etcd quorum; `MaintenanceAdmitted=False`
  (reason `QuorumFloor`) holds the rest until it reaches Ready with boot
  verified. The admission count fails closed: an unreliable count holds
  admission rather than risking a second member. The worker-role label is
  never applied to control-plane nodes.
- **Taint contract** (design §6.1): `spectrocloud.com/nodeprep:NoSchedule` is
  applied at adoption and released **only** when the node reaches Ready with
  boot-verify passed on the current boot. Because taints live in etcd, a
  post-Ready reboot can't race workloads: drift or a new boot re-verifies,
  and failure re-applies the taint.
- **Boot protocol** (§5.2): boot_id changes are detected, counted per stage,
  and force re-verification before the taint is released.
- **CAPI absorption** (§6.3): Machines matching `status.nodeRef.name` are
  paused (`cluster.x-k8s.io/paused`) while prepping and unpaused at Ready;
  a Failed NodePrep stays paused; de-adoption releases the Machine. Every
  pause/unpause transition is logged and emitted as an event — found in live
  testing, an applied-but-silent pause reads exactly like a missing one. No
  Machine CRD → logic skips itself (re-probed when `capiPause` is set).
- **Fleet windows** (§9.1): `maxConcurrentFlashes` flash admission and serial
  control-plane quorum admission, expressed as conditions the agent gates on.
- **Inventory**: PCI scan of GPUs (0x10de) and Mellanox (0x15b3) via sysfs,
  written to status; rail assignment from `spec.rails`.
- **Step ledger**: every step carries state/attempts/message; 5 failed
  attempts → phase Failed (recoverable with the
  `nodeprep.spectrocloud.com/resume` annotation).

Detect-first steps (report Blocked honestly rather than half-configure):
`downloads` is fully real (HTTP + sha256 + atomic rename); `ibCoreNetns`,
`udevRules`, `ovsBridges`
detect current host state and compare it to the profile; flashing/
mlxconfig/lossless-RoCE steps gate on Mellanox presence and MFT
tooling. With `-host-mutations` **off** (the default, mirrored by
`policy.hostMutations`), everything stays detect-only: the agent writes
status and events but does not touch the host. Mutating steps that run for
real when mutations are on: the download cache under
`/opt/spectrocloud/spcx`, DOCA package installation (`dpkg`/`apt` on the
host via `nsenter` — heavy commands run in a host systemd transient unit
(`systemd-run --wait --pipe`) so apt/dpkg memory is accounted to the host,
not the pod cgroup; found in live testing, `apt-get install doca-all` as a
pod child OOM-killed the agent — deliberately with no Mellanox-hardware
gate). Package names may carry the one supported shell-style placeholder
`$(uname -r)` — the agent expands it from the host kernel release (e.g.
`linux-headers-$(uname -r)`), holds freshly installed `linux-headers-*`
packages (`apt-mark hold`, as the bash script does) so later kernel churn
cannot strand the running kernel, and refuses any other shell expression
instead of passing it to apt. `lldpdConfig` writes
`/etc/lldpd.d/rcp-lldpd.conf` and enables (and, when the config changed,
restarts) `lldpd`; `rshimService` daemon-reloads, enables, restarts and
verifies `rshim` (skipping nodes without the unit) so the BlueField flash
path finds a live rshim; `grubParams` renders the bash-exact
`/etc/default/grub.d/90-nodeprep.cfg` (managed keys sed-stripped then
re-appended) and runs `update-grub` — with VFs requested it adds the
vendor IOMMU (`intel_iommu=on`/`amd_iommu=on`, detected from
`/proc/cpuinfo`) and `iommu=pt`, without which SR-IOV cannot work. The
full VF pipeline is real too (bash `fn_set_vfs`): `sriovNumVFs` converges
`sriov_numvfs` per function (teardown-to-0 before a count change; the
firmware ceiling `sriov_totalvfs` gates the write — too low reports
Blocked until the `mlxconfig` step's `NUM_OF_VFS` + apply-reboot lands),
`vfGuids` synthesizes node/port GUIDs and MACs from the PF's `node_guid`
(byte-for-byte the bash formulas, written colon-formatted to
`/sys/class/infiniband/<ib>/device/sriov/<vf>/{node,port,mac,policy}`
with `Follow` policy for IB links), unbinds/rebinds only the changed VFs
through `mlx5_core`, and renders the `71-persistent-net-vf.rules` /
`61-persistent-rdma-vf.rules` rename rules (`rdma_rename` +
`cma_roce_tos -t 96`) for rail-mapped VFs. Also the
`driverReadyMarker` write to `/run/mellanox/drivers`, and `kubeletState` —
the guarded stop → rm manager-state → restart from `fn_ensure_state`, plus
the boot hook: the agent renders `nodeprep-boot.service` + its script onto
the host (content-hashed, `Before=kubelet.service`, design §6.2), carrying
the kubelet reset and the two-condition-gated `mlnx_interface_mgr` wait
into every future boot.

## Safety model

Any host mutation requires **both**:
1. the agent DaemonSet runs with `-host-mutations` (and `-allow-reboot` for
   reboots), and
2. the profile sets `policy.hostMutations: true` (and `rebootEnabled: true`).

With either absent, steps report `Blocked` with the reason instead of
guessing. This is the v0.1 testing posture: run everything in a lab, watch
the NodePrep object describe what the bash script *would* have done.

## Verbose host-exec logging

By default the agent logs compactly: sweeps and tool queries whose output is
steady-state noise — the ACS `lspci`/`setpci` traffic over every PCI device,
`mlxconfig`/`flint` device queries (each dumps the device's full
configuration/image table), `mlnx_qos` readbacks — run quiet, and the step
message in the NodePrep status is the record ("disabled ACS on: …"). To see
every host exec in full while troubleshooting, either pass `-verbose` to the
agent or toggle it on a running cluster without touching the manifest:

```sh
kubectl -n nodeprep-system set env daemonset/nodeprep-agent NODEPREP_VERBOSE=true   # on
kubectl -n nodeprep-system set env daemonset/nodeprep-agent NODEPREP_VERBOSE-      # off
```

Both roll the agent pods; with verbose off again, quiet sweeps go silent.

## Updating a NodePrepProfile without conflicts

`kubectl apply -f profile.yaml` fails with *"the object has been modified;
please apply your changes to the latest version"* when the manifest — or the
`kubectl.kubernetes.io/last-applied-configuration` annotation recorded on
the live object by an earlier apply — carries a stale `resourceVersion`
(e.g. after exporting a profile from another cluster). The stale RV rides
into the merge patch as an optimistic-concurrency guard and the API server
rejects it.

Fix on the live object (one-time per profile):

```sh
kubectl annotate nodeprepprofile <name> \
  kubectl.kubernetes.io/last-applied-configuration-
```

Then keep profiles free of server-side metadata: strip `resourceVersion`,
`uid`, `creationTimestamp` and `generation` from exported manifests before
re-applying (or manage profiles with `kubectl apply --server-side -f ...`,
which ignores the annotation entirely).

## Cluster manifests are externally managed

On clusters where CRDs and RBAC are installed and enforced by an external
management system (GitOps/cluster-API-style drift reversion), the
`manifests/crd-*.yaml` and `manifests/rbac.yaml` files cannot be changed by
applying to the cluster directly — the external system reverts the drift,
and a reverted CRD silently **prunes** any spec fields it no longer knows
(nodes de-adopt with "no longer matches any profile"). Land CRD and RBAC
changes in the external system first; `kubectl apply` of those manifests is
a stopgap. For live profile edits prefer JSON patches
(`kubectl patch --type=json`), which never replay
`last-applied-configuration`.

## Try it (kind)

```sh
make image-controller image-agent          # docker.io/kreeuwijk/ai-nodeprep:0.1.45-{controller,agent}
kind load docker-image docker.io/kreeuwijk/ai-nodeprep:0.1.45-controller \
                        docker.io/kreeuwijk/ai-nodeprep:0.1.45-agent
make manifests-install                     # manifests reference the image tags
make sample                                # apply the example profile
kubectl label node <node> node.spectrocloud.com/ai-worker=true
```

`make image-multi` builds and pushes both images for amd64 and arm64 to
Docker Hub (run `docker login` first); `VERSION=... make image-controller`
overrides the version.

Watch it work:

```sh
kubectl get nodeprep -w          # PHASE / PROFILE / REBOOTS / Boot Verified
kubectl describe nodeprep <node> # step ledger, events, blocked reasons
kubectl get node <node> -o jsonpath='{.metadata.labels}' # legacy mirror
```

Without real Mellanox hardware the hardware steps complete as "skipped: no
Mellanox hardware present" and the node walks to Ready; with hardware and
mutations off it parks Blocked with the exact reason the bash script would
have been needed.

## Development

```sh
make build   # bin/controller, bin/agent
make test
make vet
```

Client-go v0.31 only — no controller-runtime, no codegen. Objects are read
through the dynamic client and decoded into the plain structs in
[api/v1alpha1/types.go](api/v1alpha1/types.go), so the API types need no
deepcopy machinery. The vendored dependency set is small enough to build
offline from the module cache.

Layout:

```
api/v1alpha1/        types (NodePrepProfile, NodePrep)
internal/phases/     phase machine + legacy label mapping
internal/k8sutil/    events, conditions, node taint/label patching
internal/controller/ adoption, decisions, CAPI pause
internal/agent/      poll loop, step ledger, inventory, step implementations
cmd/controller, cmd/agent
manifests/           CRDs, RBAC, Deployment, DaemonSet
samples/             example profile
```
