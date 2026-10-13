## Deploying an EKS FIPS cluster with Palette VerteX

Once the FIPS AMI image has been built, you can use Palette VerteX to deploy an EKS FIPS cluster:

1. Create a Palette cluster profile using the OS, K8S, CNI and CSI packs from VerteX (the CNI and CSI packs will show a green FIPS logo). Ensure the profile uses the same Kubernetes version as the version in the AMI you will use.
2. Deploy a new cluster using the profile
3. In the Nodes Config page of the deployment wizard, enable the "Override Cluster API node pool configuration" toggle and click the "Update Cluster API node pool configuration" button. Paste the following content in the YAML field:
  ```
  awsManagedMachinePool:
    spec:
      awsLaunchTemplate:
        ami:
          id: ami-00a4d35f6a376ca77        # Replace with the AMI ID of your own AL2023 FIPS image
        instanceMetadataOptions:
          httpTokens: required
          httpPutResponseHopLimit: 2
  ```
4. In the Cloud Configuration section of the node pool, enable the "Enable Nodepool Customization" toggle. In the "Disk Type" field that appears at the bottom, select "gp3".


## Notes

For GPU instances, the AMI image built with `enable_accelerator=nvidia` already includes the FIPS-ready GPU driver. Install the GPU Operator with `driver.enable: false` to leverage the existing host driver.