# Spectrocloud Nodeprep automation

Spectrocloud Nodeprep ensures that Kubernetes nodes become AI-capable:
* Installs NVIDIA DOCA packages
* Upgrades the firmware on NVIDIA Bluefield-3 SuperNICs and DPUs
* Configures NVIDIA Bluefield-3 SuperNICs, regular ConnectX-4/5/6/7/8 adapters and Bluefield-3 DPUs
* Configures to OS to properly handle these adapters for various scenarios:
  * Infiniband and Ethernet (RoCE) use cases
  * SRIOV scenarios with multiple VFs
  * Spectrum-X 1.x scenarios (HostDeviceNetwork)
  * Spectrum-X 2.x scenarios (Spectrum-X Operator)

## Nodeprep content from MAAS
Spectrocloud's Nodeprep process grabs the files it needs from the MAAS TFTP server. This eases deployment in airgapped environments and prevents lenghty internet downloads for DOCA on every deployment.

To set up the file structure:
* Navigate to `/var/snap/maas/common/maas/tftp_root` on the MAAS server(s)
* Create the directory structure: `mkdir -p rcp/firmware/bfb`
* Download the content and place it in the right locations:

```
/var/snap/maas/common/maas/tftp_root/
└── rcp/
    ├── doca-host_3.3.0-088000-26.01-ubuntu2204_amd64.deb
    ├── doca-host_3.3.0-088000-26.01-ubuntu2404_amd64.deb
    ├── nodeprep_v105.sh
    ├── nodeprep_v106.sh
    │
    └── firmware/
        └── bfb/
            └── bf-fwbundle-3.3.0-202_26.01-prod.bfb
```

Retrieve the DOCA Host package from this [NVIDIA website](https://developer.nvidia.com/doca-downloads?deployment_platform=Host-Server&deployment_package=DOCA-Host&target_os=Linux&Architecture=x86_64&Profile=doca-all&Distribution=Ubuntu&version=24.04&installer_type=deb_local)

Retrieve the BFB firmware package from this [NVIDIA website](https://developer.nvidia.com/doca-downloads?deployment_platform=BlueField&deployment_package=BF-FW-Bundle&installer_type=BFB)

The Nodeprep scripts are found in this repo.