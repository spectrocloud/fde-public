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

- **Adoption**: nodes matching a profile's `nodeSelector` get a NodePrep; the
  legacy `spectrocloud.com/nodeprep` label is imported and mirrored
  (`policy.labelCompat: v1`).
- **Taint contract** (design §6.1): `spectrocloud.com/nodeprep:NoSchedule` is
  applied at adoption and released **only** when the node reaches Ready with
  boot-verify passed on the current boot. Because taints live in etcd, a
  post-Ready reboot can't race workloads: drift or a new boot re-verifies,
  and failure re-applies the taint.
- **Boot protocol** (§5.2): boot_id changes are detected, counted per stage,
  and force re-verification before the taint is released.
- **CAPI absorption** (§6.3): Machines matching `status.nodeRef.name` are
  paused (`cluster.x-k8s.io/paused`) while prepping and unpaused at Ready;
  a Failed NodePrep stays paused. No Machine CRD → logic skips itself.
- **Fleet windows** (§9.1): `maxConcurrentFlashes` flash admission and serial
  control-plane quorum admission, expressed as conditions the agent gates on.
- **Inventory**: PCI scan of GPUs (0x10de) and Mellanox (0x15b3) via sysfs,
  written to status; rail assignment from `spec.rails`.
- **Step ledger**: every step carries state/attempts/message; 5 failed
  attempts → phase Failed (recoverable with the
  `nodeprep.spectrocloud.com/resume` annotation).

Detect-first steps (report Blocked honestly rather than half-configure):
`downloads` is fully real (HTTP + sha256 + atomic rename); `grubParams`,
`ibCoreNetns`, `sriovNumVFs`, `udevRules`, `ovsBridges`, `kubeletState`
detect current host state and compare it to the profile; flashing/
mlxconfig/lossless-RoCE/VF-GUID steps gate on Mellanox presence and MFT
tooling. With `-host-mutations` **off** (the default, mirrored by
`policy.hostMutations`), everything stays detect-only: the agent writes
status and events but does not touch the host. Two mutating steps already
run for real when mutations are on: the `driverReadyMarker` write to
`/run/mellanox/drivers` and the download cache under `/opt/spectrocloud/spcx`.

## Safety model

Any host mutation requires **both**:
1. the agent DaemonSet runs with `-host-mutations` (and `-allow-reboot` for
   reboots), and
2. the profile sets `policy.hostMutations: true` (and `rebootEnabled: true`).

With either absent, steps report `Blocked` with the reason instead of
guessing. This is the v0.1 testing posture: run everything in a lab, watch
the NodePrep object describe what the bash script *would* have done.

## Try it (kind)

```sh
make image-controller image-agent          # docker.io/kreeuwijk/ai-nodeprep:0.1.4-{controller,agent}
kind load docker-image docker.io/kreeuwijk/ai-nodeprep:0.1.4-controller \
                        docker.io/kreeuwijk/ai-nodeprep:0.1.4-agent
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
