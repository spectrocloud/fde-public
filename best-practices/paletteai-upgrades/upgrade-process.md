## PaletteAI upgrades

PaletteAI has its own pages for upgrade guidance:
* [Overall upgrade page](https://docs.palette-ai.com/docs/installation-and-setup/upgrade)
* [Management Appliance upgrade specifics](https://docs.palette-ai.com/docs/installation-and-setup/upgrade/#upgrade-appliance)

The biggest takeaway is that PaletteAI *does not get upgraded automatically* after pushing a new content bundle to the management appliance and triggering the upgrade process. Only the nodes, middleware and Palette/Vertex itself will get upgraded.

> [!NOTE]
> The reasoning behind this is that the PaletteAI team expects customers to make changes to the PaletteAI HelmRelease object, which would get lost by an automatic upgrade. This may however change in the future.

### Performing the PaletteAI upgrade after upgrading the management appliance

1. Suspend the `mural` HelmRelease so that you can make changes without triggering an upgrade
   ```
   kubectl patch helmrelease mural --namespace mural-system --type merge --patch '{"spec":{"suspend":true}}'
   ```
2. Update the `mural-crds` and `mural` OCIRepository resources to point to the newer versions
   ```
   # For PaletteAI 1.4.0:
   kubectl patch ocirepository mural-crds --namespace mural-system --type merge --patch '{"spec":{"ref":{"semver":"0.7.26"}}}'
   kubectl patch ocirepository mural      --namespace mural-system --type merge --patch '{"spec":{"ref":{"semver":"1.4.0"}}}'
   ```
3. Edit the `mural` HelmRelease and update its values as directed [here](https://docs.palette-ai.com/docs/release-notes/palette-ai-v1.4.0/#mural-helm-values) (currently broken, waiting for better guidance)
   ```
   kubectl edit helmrelease mural --namespace mural-system
   ```
4. Resume the `mural` HelmRelease so that you the change can trigger an upgrade
   ```
   kubectl patch helmrelease mural --namespace mural-system --type merge --patch '{"spec":{"suspend":false}}'
   ```

### Other considerations

Manual wipes of PaletteAI will encounter some unexpected issues due to the following architectural design choices made in the management appliance variant of PaletteAI:

- The template `values.yaml` for PaletteAI is shipped with the appliance image in `/usr/share/palette-ai/values.default.yaml` but isn't used from that location. When PaletteAI is first deployed, that file is copied to `/usr/local/palette-ai/values.yaml` and then never changes ever again.
- The installer then uses `/usr/local/palette-ai/values.yaml` is input for installation of the `HelmRelease`.
- Subsequent upgrades don't use either of those files, the user is expected to modify the `HelmRelease` directly.
- The actual final values for the `HelmRelease` are not the raw contents of `/usr/local/palette-ai/values.yaml`, so you *cannot* do a simple copy/paste of `/usr/share/palette-ai/values.default.yaml` into the `HelmRelease` for upgrades. *Doing so will break PaletteAI*.

> [!NOTE]
> Better guidance from the PaletteAI team is coming on what values to adjust in the HelmRelease on upgrade.

Manually uninstalling PaletteAI (for whatever reason) in order for a new fresh deployment to happen, will encounter problems due to the situation above: the `/usr/local/palette-ai/values.yaml` file may well be for a version of PaletteAI that is several versions older than the version you're reinstalling now.

In order to handle this properly upon a manual wipe:
* Copy the `/usr/share/palette-ai/values.default.yaml` file to `/usr/local/palette-ai/values.yaml` again to get the latest starting values
* Uninstall PaletteAI completely
* Let the management appliance reinstall PaletteAI using the fresh new values.