# Upgrading to Kubernetes 1.33.12, 1.34.8, 1.34.5, 1.36.1 or newer

## Cluster API
Upgrading to the latest patch release of Kubernetes 1.33/1.34/1.35/1.36 with Cluster API requires a fix from CAPI version 1.13.2 to work smoothly.
In order to get this CAPI fix, Palette must be upgraded to version 4.9.44.

A workaround to upgrade to Kubernetes to these patch releases with older versions of Palette is to manually create the following resource in the cluster prior to upgrading:
```
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeadm:apiserver-kubelet-client
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:kubelet-api-admin
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: User
  name: kube-apiserver-kubelet-client
```


## Edge Agent/Appliance Mode
There is a known issue with 1.35.x on Edge in airgapped environments, from the docs page:

On Edge clusters deployed in airgap mode at Kubernetes v1.35.x or later, on all three Edge Kubernetes distributions (Palette Optimized K3s, Palette Optimized RKE2, and Palette eXtended Kubernetes Edge (PXK-E)), pods may intermittently fail to start with an ImagePullBackOff status even though the image is already present on the node. The palette-webhook and palette-lite-controller-manager pods are the most commonly affected. Kubernetes v1.35 enables the KubeletEnsureSecretPulledImages feature gate, which can record an image loaded from the Palette content bundle without credential information and force Kubelet to attempt a new pull from the image's original public registry. On K3s and RKE2, pack version 1.35.6 includes an Airgap preset that resolves the issue, but the preset is not enabled by default, so clusters on 1.35.6 remain affected until you enable it, and pack versions 1.35.2 and 1.35.3 do not include it at all. On PXK-E, no released pack version includes the preset, so the setting must be applied as a values override, which additionally requires setting the Kubelet --config-dir argument.

The workaround for PXK-E is to include the following content in the Kubernetes layer:
```
kubeadmconfig:
  kubeletExtraArgs:
    config-dir: "/etc/kubernetes/kubelet.conf.d"

stages:
  initramfs:
    - files:
        - path: /etc/kubernetes/kubelet.conf.d/10-image-pull-creds.conf
          permissions: 0600
          content: |
            apiVersion: kubelet.config.k8s.io/v1beta1
            kind: KubeletConfiguration
            featureGates:
              KubeletEnsureSecretPulledImages: true
            imagePullCredentialsVerificationPolicy: NeverVerify
```

More info here: https://docs.spectrocloud.com/troubleshooting/edge/#scenario---intermittent-imagepullbackoff-errors-on-airgap-edge-clusters-with-kubernetes-v135x
