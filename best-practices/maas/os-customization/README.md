# MAAS OS customization during deployment

When deploying Palette clusters with MAAS, there are several things that are good to handle automatically:

1. Pinning the kernel to a specific version
2. Blacklisting the Nouveau driver
3. Disable TX offloading on QEMU systems (libvirt, Proxmox)
4. Remove the `/etc/apt/sources.list.d/ubuntu.sources` file from deployed images to ensure only sources defined in MAAS are applied
5. Adjust the MAAS server IP in cloud-init and apt proxy config

See the [curtin_userdata_custom](./curtin_userdata_custom) as a full example. Note that this file only applies to custom (CAPI) images.
If you want to use the same Curtin userdata for vanilla Ubuntu deployments, create a copy named `curtin_userdata_ubuntu`.

## Pinning the kernel to a specific version

This is achieved through:
```
kernel:
  package: linux-image-6.17.0-29-generic
  flavor: hwe
```
and a late command to install the `linux-modules-extra` module for that kernel version:
```
late_commands:
  2_extra_modules: ["curtin", "in-target", "--", "apt", "install", "-y", "--allow-change-held-packages", "linux-modules-extra-6.17.0-29-generic"]
```
The `linux-modules-extra` module install is required because only the `linux-image-generic` metapackage includes additional packages, including `linux-modules-extra`. When the metapackage isn't used and a specific kernel image is specified as above, the other packages are no longer included. We don't care about most of those, but the `linux-modules-extra` package provides much wider hardware support that is often a prerequisite on modern hardware.

## Blacklisting the Nouveau driver

This driver can conflict with the Nvidia driver, or prevent systems from rebooting cleanly at all. When deploying systems with GPUs (especially RTX PRO 6000), it's good to prevent this driver from loading.
It is achieved through:
```
write_files:
  blacklist_nouveau:
    path: /etc/modprobe.d/blacklist-nouveau.conf
    content: |
      blacklist nouveau
      blacklist lbm-nouveau
      options nouveau modeset=0
      alias nouveau off
      alias lbm-nouveau off
    permissions: '0644'
```

## Disable TX offloading on QEMU systems (Proxmox)

Send (TX) offloading for tunneling protocols like VXLAN and Geneve is broken on the Virtio driver used in Proxmox environments on some hardware. We have seen this specifically on HPE servers so far.

The issues can be prevented through:
```
write_files:
  disable_offload_proxmox:
    path: /usr/lib/networkd-dispatcher/routable.d/10-disable-offloading
    content: |
      #!/bin/sh
      ethtool --offload $IFACE tx off
    permissions: '0755'
```
and a late command to ensure this workaround is only implemented on Proxmox VMs:
```
late_commands:
  4_no_proxmox: curtin in-target -- /bin/bash -c 'if [ "$(cat /sys/class/dmi/id/chassis_vendor)" != "QEMU" ]; then rm /usr/lib/networkd-dispatcher/routable.d/10-disable-offloading; fi'
```

## Remove the /etc/apt/sources.list.d/ubuntu.sources file

CAPI images often comes with a `ubuntu.sources` file this left on it from the original builder.
Those sources may not be desirable for the end customer. MAAS places a `/etc/apt/sources.list` on the node during deployment, containing only the sources configured by the MAAS administrator. Hence it makes sense to delete the `ubuntu.sources` file from the node.

It is achieved through:
```
late_commands:
  3_clean_sources: ["curtin", "in-target", "--", "rm", "-f", "/etc/apt/sources.list.d/ubuntu.sources"]
```

## Adjust the MAAS server IP for the node

After deployment, the node has 2 references back to MAAS defined:
1. Cloud-init points at MAAS to confirm successful deployment and retrieve the post-deploy cloud-init metadata
2. APT is configured with a proxy pointing to MAAS, if MAAS is configured to act as an APT proxy for nodes

Both settings default to the MAAS IP address used during the PXE installation phase. In some environments, this IP address should not be used after the PXE phase completes. This is because the original IP either:
* Unreachable after deployment (e.g. when the PXE network IP is gone after the reboot)
* Undesired as it's non-HA and a load-balanced IP exists that would provide higher availability

We can fix this during the PXE phase before the node reboots:
```
late_commands:
  5_change_maas_ip_1: curtin in-target -- sh -c 'set -x ; sed -i "s/192.168.10.1[0|1]/10.0.20.30/g" /etc/cloud/cloud.cfg.d/*'
  6_change_maas_ip_2: curtin in-target -- sh -c 'set -x ; sed -i "s/192.168.10.1[0|1]/10.0.20.30/g" /etc/apt/apt.conf.d/*
```
In this example, `192.168.10.1[0|1]` replaces either `192.168.10.10` or `192.168.10.11` with a new MAAS IP (`10.0.20.30`). If you have an MAAS HA setup with 2 servers, this captures both possible source IPs in the search.