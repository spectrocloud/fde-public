# nodeprep-controller

Checks Kubernetes nodes for a `spectrocloud.com/nodeprep` label and if the label has a value of "completed", removes the `spectrocloud.com/nodeprep:NoSchedule` taint from that node. Useful for preventing workloads from landing onto a node before all the prereq actions for the node are completed.

## Deployment

Deploy to a cluster through a manifest like so:

```
apiVersion: v1
kind: Namespace
metadata:
  name: nodeprep-controller
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: nodeprep-controller
  namespace: nodeprep-controller
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: nodeprep-controller
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch", "update"] # update needed for taint removal
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: nodeprep-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: nodeprep-controller
subjects:
  - kind: ServiceAccount
    name: nodeprep-controller
    namespace: nodeprep-controller
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nodeprep-controller
  namespace: nodeprep-controller
spec:
  replicas: 1
  selector:
    matchLabels:
      app: nodeprep-controller
  template:
    metadata:
      labels:
        app: nodeprep-controller
    spec:
      serviceAccountName: nodeprep-controller
      # --- schedule only on control-plane nodes ---
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              # Terms are OR'ed. Either label presence will match.
              - matchExpressions:
                  - key: node-role.kubernetes.io/control-plane
                    operator: Exists
              - matchExpressions:
                  - key: node-role.kubernetes.io/master
                    operator: Exists
      tolerations:
        # Tolerate the default control-plane taints (older & newer).
        # Using operator: Exists lets us match regardless of effect.
        - key: node-role.kubernetes.io/control-plane
          operator: Exists
        - key: node-role.kubernetes.io/master
          operator: Exists
      containers:
        - name: controller
          image: kreeuwijk/nodeprep-controller:v1.0.0
          imagePullPolicy: IfNotPresent
          resources:
            requests: { cpu: "50m", memory: "64Mi" }
            limits:   { cpu: "200m", memory: "128Mi" }
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532         # <-- distroless "nonroot"
        runAsGroup: 65532        # <-- distroless "nonroot"
        seccompProfile:
          type: RuntimeDefault
```
