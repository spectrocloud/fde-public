# Deploying OpenShell with Palette

In order to deploy OpenShell with Palette, we need 3 packs:
1. KGateway 2.2.2
2. Agent Sandbox 1.0.10
3. NVIDIA OpenShell 0.1.8

KGateway is available on the public Palette Registey. The other two have been [submitted](https://github.com/spectrocloud/pack-central/pull/366) to the Community Registry and are already available on Kevin's private registry.

The KGateway and Agent Sandbox packs are prerequisites for OpenShell, they should be deployed first with default values.

## Deploying the OpenShell pack

This pack needs adjustments from the default values:

* Under `charts.helm-chart.sandbox.image`:
  * Set `.repository` and `.tag` to a useful sandbox image. More info on that below.
* Under `charts.helm-chart.server`:
  * Set `.drivers.kubernetes.workspaceMode` to `"managed"`
  * Set `.disableTls` to `true`
  * Set `.oidc.issuer` to a valid OIDC issuer. More info on that below.
* Under `charts.helm-chart.grpcRoute`:
  * Set `.enabled` to `true`
  * Set `.hostname` to a list of valid FQDN(s) for the OpenShell gateway
  * Set `.gateway.create` to `true`
  * Set `.gateway.className` to `kgateway`
  * Set `.gateway.name` to `"openshell-ingress"`
  * Set `.gateway.listener.port` to `443`
  * Set `.gateway.listener.protocol` to `"HTTPS"`
  * Set `.gateway.listener.tls.certificateRefs` to `- name: "openshell-gw-tls"`, referencing a secret name that contains a trusted SSL certificate that is valid for the name(s) in `charts.helm-chart.grpcRoute.hostname`
* Add a manifest to the OpenShell pack layer that defines the secret referenced in `charts.helm-chart.grpcRoute.gateway.listener.tls.certificateRefs`
  For example:
  ```
  apiVersion: v1
  data:
    tls.crt: LS0tLS1CRU...tLS0tLQo=
    tls.key: LS0tLS1CRU...ZLS0tLS0K
  kind: Secret
  metadata:
    name: openshell-gw-tls
    namespace: openshell
  type: Opaque
  ```

### OIDC Issuer

PowerShell should always be used with an OIDC issuer. It is possible to use Keycloak from PaletteAI Inference Launchpad for this, but it can also be something else.
We will use Keycloak from PaletteAI Inference Launchpad for this guide.

The OpenShell realm needs to be imported into Keycloak first. Download [keycloak-realm.json](https://raw.githubusercontent.com/NVIDIA/OpenShell/refs/heads/main/scripts/keycloak-realm.json) from the OpenShell repo and import it into Keycloak:
1. Log into PAIL's Keycloak with the PAIL admin account: https://<pail-fqdn>/iam
2. Click `Manage realms --> Create realm` and upload the downloaded keycloak-realm.json file
3. Click `Create`. This also switches you to the newly created `openshell` realm.
4. Navigate to `Users`. As you can see, it comes with some example users, passwords are in the keycloak-realm.json file.
5. To create new admins, associate them to both the `openshell-admin` and `openshell-user` realm roles.
6. To create new users, associate them to just the `openshell-user` realm role.

Once Keycloak is set up, set `charts.helm-chart.server.oidc.issuer` to the openshell realm in PAILs Keycloak: `"https://<pail-fqdn>/iam/realms/openshell"`.

### Sandbox image

Openshell 0.1.8 uses `nvcr.io/nvidia/base/ubuntu:24.04` as the default sandbox image, which isn't very useful. However the [OpenShell Community sandboxes](https://github.com/NVIDIA/OpenShell-Community/tree/main/sandboxes) now are no longer maintained and contain a policy that is incompatible with the latest OpenShell.

Also, since we want to integrate with PAIL, we need a few extra changes anyway. As a result, we need to make our own images.
The content needed to build an adjusted sandbox-base image is in [sandbox-pail](./sandbox-pail/). It contains the following changes compared to the community image:
* The policy.yaml is adjusted to remove the use of `tls: terminate`, as `terminate` is no longer a supported value for the `tls` parameter.
* A `claude.json` is added to get the Claude Code CLI to start without any setup prompts
* A `merge-claude-config.py` script is added to merge dynamically created provider config into `~/.claude.json` so that Claude is pointed to PAIL and using the right model.
* The Dockerfile is adjusted to:
  * Use ubuntu:26.04 as the base image instead of an NGC image that requires a pull secret
  * Set the UID and GID of the `sandbox` user to `10001`, which is how OpenShell on Kubernetes runs all sandboxes
  * Not create the `sandbox` user as a system account. The supervisor account likely is also not needed, but left in for completeness.
  * Handle the 3 adjustments mentioned above

Build and push the sandbox image:
```
docker build . -t my.registry.com/openshell-sandbox-pail:1.0.0 --progress=plain --push
```

Then:
- Set `charts.helm-chart.sandbox.image.repository` to `"my.registry.com/openshell-sandbox-pail"`
- Set `charts.helm-chart.sandbox.image.tag` to `"1.0.0"`

## Post deployment setup

After deploying the packs, follow this procedure to setup OpenShell with PAIL.
1. Download and install the OpenShell CLI [Mac](https://github.com/NVIDIA/OpenShell/releases/download/v0.1.8/openshell-aarch64-apple-darwin.tar.gz) / [Linux](https://github.com/NVIDIA/OpenShell/releases/download/v0.1.8/openshell-x86_64-unknown-linux-musl.tar.gz)
2. Register the OpenShell Gateway you've deployed
   ```
   openshell gateway add https://<openshell-gateway-fqdn> --name k8s --oidc-issuer https://<pail-fqdn>/iam/realms/openshell --oidc-client-id openshell-cli
   ```
   Note that `--oidc-issuer` anf `--oidc-client-id` are **not** optional, it will not work without specifying them.
3. Create a workspace to work in (best to not use the `default` workspace)
   ```
   openshell workspace create --name claude
   ```
3. Import a Provider Profile for PAIL. An example is provider [here](./provider-profiles/provider-profile-pail-custeng.yaml). Make sure to adjust at least `endpoints.host` to your own PAIL instance FQDN.
   ```
   openshell --workspace claude profile import --file provider-profile-pail-custeng.yaml
   ```
4. Create a provider from the profile, including the desired configuration values
   ```
   openshell --workspace claude provider create \
     --name pail-claude \
     --type pail-claude \
     --credential ANTHROPIC_AUTH_TOKEN=your-pail-token-here \
     --config ANTHROPIC_BASE_URL="https://pail.company.com" \
     --config ANTHROPIC_MODEL="ds-4.1-flash[1m]" \
     --config CLAUDE_CODE_AUTO_MODE_SERVER=0 \
     --config CLAUDE_CODE_EFFORT_LEVEL="xhigh" \
     --config CLAUDE_CODE_AUTO_COMPACT_WINDOW=984576
   ```

## Creating a sandbox

Now you're ready to deploy sandboxes, Claude Code should work without any manual action.

`interactive (run "claude" to start Claude Code)`
```
openshell --workspace claude sandbox create --name pail-sbx1 --provider pail-claude
```

`opening claude directly`
```
openshell --workspace claude sandbox create --name pail-sbx2 --provider pail-claude -- bash -ic 'claude'
```

`one-shot with automatic removal`
```
openshell --workspace claude sandbox create --name pail-sbx3 --provider pail-claude --no-keep -- bash -ic 'claude -p "what model are you?"'
```

The reason we use `bash -ic` here is because we need `~/.bashrc` to run so that `~/.claude.json` gets augmented with the additional configuration parameters to point Claude Code to PAIL. Once OpenShell ships support for defining Environment variables in Provider Profiles directly (expoected for 2.0.0), this workaround can be replaced with env vars and the `bash -ic` requirement disappears.