# hybrid-csi-plugin

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.0.1](https://img.shields.io/badge/AppVersion-v0.0.1-informational?style=flat-square)

Container Storage Interface plugin

The Container Storage Interface (CSI) plugin is a specification designed to standardize the way container orchestration systems like Kubernetes, interact with different storage systems. The CSI plugin abstracts the underlying storage, enabling the seamless integration of different storage solutions (such as local block devices, file systems, or cloud-based storage) with containerized applications.

**Homepage:** <https://github.com/sergelogvinov/hybrid-csi-plugin>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| sergelogvinov |  | <https://github.com/sergelogvinov> |

## Source Code

* <https://github.com/sergelogvinov/hybrid-csi-plugin>

## Deploy

```shell
# Prepare namespace
kubectl create ns csi-hybrid

# Install hybrid CSI hybrid
helm upgrade -i --namespace=csi-hybrid -f hybrid-csi.yaml \
    hybrid-csi-plugin oci://ghcr.io/sergelogvinov/charts/hybrid-csi-plugin
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| replicaCount | int | `1` |  |
| imagePullSecrets | list | `[]` |  |
| nameOverride | string | `""` |  |
| fullnameOverride | string | `""` |  |
| createNamespace | bool | `false` | Create namespace. Very useful when using helm template. |
| priorityClassName | string | `"system-cluster-critical"` | Controller pods priorityClassName. |
| serviceAccount | object | `{"annotations":{},"create":true,"name":""}` | Pods Service Account. ref: https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/ |
| provisionerName | string | `"csi.hybrid.sinextra.dev"` | CSI Driver provisioner name. Currently, cannot be customized. |
| clusterID | string | `"kubernetes"` | Cluster name. Currently, cannot be customized. |
| logVerbosityLevel | int | `5` | Log verbosity level. See https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md for description of individual verbosity levels. |
| timeout | string | `"3m"` | Connection timeout between sidecars. |
| storageClass | list | `[]` | Storage class definition. |
| controller.podAnnotations | object | `{}` | Annotations for controller pod. ref: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations/ |
| controller.plugin.image | object | `{"pullPolicy":"IfNotPresent","repository":"ghcr.io/sergelogvinov/hybrid-csi-controller","tag":""}` | Controller CSI Driver. |
| controller.plugin.resources | object | `{"requests":{"cpu":"10m","memory":"16Mi"}}` | Controller resource requests and limits. ref: https://kubernetes.io/docs/user-guide/compute-resources/ |
| controller.attacher.image | object | `{"pullPolicy":"IfNotPresent","repository":"registry.k8s.io/sig-storage/csi-attacher","tag":"v4.4.4"}` | CSI Attacher. |
| controller.attacher.resources | object | `{"requests":{"cpu":"10m","memory":"16Mi"}}` | Attacher resource requests and limits. ref: https://kubernetes.io/docs/user-guide/compute-resources/ |
| controller.provisioner.image | object | `{"pullPolicy":"IfNotPresent","repository":"registry.k8s.io/sig-storage/csi-provisioner","tag":"v3.6.4"}` | CSI Provisioner. |
| controller.provisioner.resources | object | `{"requests":{"cpu":"10m","memory":"16Mi"}}` | Provisioner resource requests and limits. ref: https://kubernetes.io/docs/user-guide/compute-resources/ |
| controller.resizer.image | object | `{"pullPolicy":"IfNotPresent","repository":"registry.k8s.io/sig-storage/csi-resizer","tag":"v1.9.4"}` | CSI Resizer. |
| controller.resizer.resources | object | `{"requests":{"cpu":"10m","memory":"16Mi"}}` | Resizer resource requests and limits. ref: https://kubernetes.io/docs/user-guide/compute-resources/ |
| node.plugin.image | object | `{"pullPolicy":"IfNotPresent","repository":"ghcr.io/sergelogvinov/hybrid-csi-node","tag":""}` | Node CSI Driver. |
| node.plugin.resources | object | `{}` | Node CSI Driver resource requests and limits. ref: https://kubernetes.io/docs/user-guide/compute-resources/ |
| node.driverRegistrar.image | object | `{"pullPolicy":"IfNotPresent","repository":"registry.k8s.io/sig-storage/csi-node-driver-registrar","tag":"v2.9.4"}` | Node CSI driver registrar. |
| node.driverRegistrar.resources | object | `{"requests":{"cpu":"10m","memory":"16Mi"}}` | Node registrar resource requests and limits. ref: https://kubernetes.io/docs/user-guide/compute-resources/ |
| node.kubeletDir | string | `"/var/lib/kubelet"` | Location of the /var/lib/kubelet directory as some k8s distribution differ from the standard. Standard: /var/lib/kubelet, k0s: /var/lib/k0s/kubelet, microk8s: /var/snap/microk8s/common/var/lib/kubelet |
| node.nodeSelector | object | `{}` | Node labels for node-plugin assignment. ref: https://kubernetes.io/docs/user-guide/node-selection/ |
| node.tolerations | list | `[{"effect":"NoSchedule","key":"node.kubernetes.io/unschedulable","operator":"Exists"},{"effect":"NoSchedule","key":"node.kubernetes.io/disk-pressure","operator":"Exists"}]` | Tolerations for node-plugin assignment. ref: https://kubernetes.io/docs/concepts/configuration/taint-and-toleration/ |
| livenessprobe.image | object | `{"pullPolicy":"IfNotPresent","repository":"registry.k8s.io/sig-storage/livenessprobe","tag":"v2.11.0"}` | Common livenessprobe sidecar. |
| livenessprobe.failureThreshold | int | `5` | Failure threshold for livenessProbe |
| livenessprobe.initialDelaySeconds | int | `10` | Initial delay seconds for livenessProbe |
| livenessprobe.timeoutSeconds | int | `10` | Timeout seconds for livenessProbe |
| livenessprobe.periodSeconds | int | `60` | Period seconds for livenessProbe |
| livenessprobe.resources | object | `{"requests":{"cpu":"10m","memory":"16Mi"}}` | Liveness probe resource requests and limits. ref: https://kubernetes.io/docs/user-guide/compute-resources/ |
| initContainers | list | `[]` | Add additional init containers for the CSI controller pods. ref: https://kubernetes.io/docs/concepts/workloads/pods/init-containers/ |
| hostAliases | list | `[]` | hostAliases Deployment pod host aliases ref: https://kubernetes.io/docs/tasks/network/customize-hosts-file-for-pods/ |
| podAnnotations | object | `{}` | Annotations for controller pod. ref: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations/ |
| podSecurityContext | object | `{"fsGroup":65532,"fsGroupChangePolicy":"OnRootMismatch","runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532}` | Controller Security Context. ref: https://kubernetes.io/docs/tasks/configure-pod-container/security-context/#set-the-security-context-for-a-pod |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Controller Container Security Context. ref: https://kubernetes.io/docs/tasks/configure-pod-container/security-context/#set-the-security-context-for-a-pod |
| updateStrategy | object | `{"rollingUpdate":{"maxUnavailable":1},"type":"RollingUpdate"}` | Controller deployment update strategy type. ref: https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#updating-a-deployment |
| metrics | object | `{"enabled":false,"port":8080,"type":"annotation"}` | Prometheus metrics |
| metrics.enabled | bool | `false` | Enable Prometheus metrics. |
| metrics.port | int | `8080` | Prometheus metrics port. |
| nodeSelector | object | `{}` | Node labels for controller assignment. ref: https://kubernetes.io/docs/user-guide/node-selection/ |
| tolerations | list | `[]` | Tolerations for controller assignment. ref: https://kubernetes.io/docs/concepts/configuration/taint-and-toleration/ |
| affinity | object | `{}` | Affinity for controller assignment. ref: https://kubernetes.io/docs/concepts/configuration/assign-pod-node/#affinity-and-anti-affinity |
| extraVolumes | list | `[]` | Additional volumes for Pods |
| extraVolumeMounts | list | `[]` |  |
