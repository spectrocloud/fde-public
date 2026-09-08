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
```

## What it deploys

| Template | Kind | Notes |
|---|---|---|
| `namespace.yaml` | Namespace | PSA `privileged` (agent needs it). Kept on uninstall. Skippable via `namespace.create=false`. |
| `crds/` | 2 CRDs | Standard `crds/` directory: installed at `helm install`, **not** auto-upgraded by `helm upgrade` — bump CRDs through the manifests flow (Palette) or delete/reinstall. |
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

`Chart.yaml` `appVersion` tracks the release (e.g. `0.1.80`); both image tags
derive from it. Bump `appVersion` (and, for chart-shape changes, `version`)
together with the manifests version bump.

## Sync helper

The CRD copies under `crds/` must stay byte-identical to `manifests/crd-*.yaml`:

```sh
make chart-sync-crds
```
