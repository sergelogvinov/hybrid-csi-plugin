# Install plugin

## Requirements

* The backend StorageClasses already work on their own: each one provisions volumes for the nodes it serves.
* The hybrid StorageClass uses `volumeBindingMode: WaitForFirstConsumer`.
* Backends should provision for the node in `volume.kubernetes.io/selected-node`. Every external-provisioner based CSI driver and local-path-provisioner does. A volume that does not fit the selected node is discarded and the PVC is rescheduled.

## Install CSI Driver

Create a namespace `csi-hybrid` for the plugin

```shell
kubectl create ns csi-hybrid
```

### Install the plugin by using kubectl

Install latest release version

```shell
kubectl apply -f https://raw.githubusercontent.com/sergelogvinov/hybrid-csi-plugin/refs/heads/main/docs/deploy/hybrid-csi-plugin-release.yml
```

Or install latest stable version (edge)

```shell
kubectl apply -f https://raw.githubusercontent.com/sergelogvinov/hybrid-csi-plugin/refs/heads/main/docs/deploy/hybrid-csi-plugin.yml
```

### Install the plugin by using Helm

Create the helm values file, for more information see [values.yaml](../charts/hybrid-csi-plugin/values.yaml)

```yaml
# Run the plugin in the control plane
nodeSelector:
  node-role.kubernetes.io/control-plane: ""
tolerations:
  - key: node-role.kubernetes.io/control-plane
    effect: NoSchedule

# Define the storage classes
storageClass:
  - name: hybrid
    default: true
    storageClasses: proxmox,hcloud-volumes
```

Install the plugin. You need to prepare the `csi-hybrid` namespace first, see above

```shell
helm upgrade -i -n csi-hybrid -f hybrid-csi.yaml hybrid-csi-plugin oci://ghcr.io/sergelogvinov/charts/hybrid-csi-plugin
```

### Install the plugin by using Talos machine config

If you're running [Talos](https://www.talos.dev/) you can install Hybrid CSI plugin using the machine config

```yaml
cluster:
  externalCloudProvider:
    enabled: true
    manifests:
      - https://raw.githubusercontent.com/sergelogvinov/hybrid-csi-plugin/refs/heads/main/docs/deploy/hybrid-csi-plugin.yml
```

## Configuration

Controller flags and the matching Helm values:

| Flag | Helm value | Default | Description |
|------|------------|---------|-------------|
| `--helper-timeout` | | `10m` | How long a helper PVC may stay pending before the PVC is rescheduled to another node |
| `--helper-max-reschedules` | | `3` | How many times a PVC may be rescheduled because of the helper timeout. After that only `HelperTimeout` events are emitted |
| `--fix-reclaim-policy` | | `false` | Fix the reclaim policy of volumes provisioned by v0.x, see [Upgrade from v0.x](#upgrade-from-v0x) |
| `--http-endpoint` | `metrics.enabled`, `metrics.port` | disabled | Address of the metrics endpoint, see [metrics](metrics.md) |
| `--retry-interval-start`, `--retry-interval-max` | `extraArgs` | `1s`, `5m` | Backoff of the provisioning passes while the backend is provisioning |
| `--lifecycle-workers` | | `1` | Workers of the lifecycle controller |

A short `--helper-timeout` moves pods away from a broken backend faster, but a slow backend (cloud volumes, Ceph under load) may then be given up too early. Keep it above the normal provisioning time of your slowest backend.

## ResourceQuota

While a volume is being provisioned, the namespace holds **two** PVCs for it: your PVC (hybrid class) and the helper (backend class). Kubernetes counts both against the namespace quota until the helper is deleted. Once provisioning is done, only your PVC is counted, under the hybrid class.

| Quota | Counted during provisioning | What to allow |
|-------|-----------------------------|---------------|
| `requests.storage` | your PVC + helper | about **2×** the largest volume provisioned at once (more if many PVCs are created together, for example a StatefulSet with `podManagementPolicy: Parallel`) |
| `persistentvolumeclaims` | your PVC + helper | **+1** for each PVC being provisioned at the same time |
| `<hybrid>.storageclass.storage.k8s.io/requests.storage` | your PVC | the normal size, this is what is counted long-term |
| `<backend>.storageclass.storage.k8s.io/requests.storage` | helper, briefly | the request size for **every** backend in the list, otherwise creating the helper fails |
| `<backend>.storageclass.storage.k8s.io/persistentvolumeclaims` | helper, briefly | at least 1 for every backend in the list |

If the quota rejects the helper, your PVC gets a `Warning ProvisioningFailed` event with the quota error, and the plugin keeps retrying.

## Upgrade from v0.x

Upgrade the Helm release as usual. The new version needs `update` on PersistentVolumes and PersistentVolumeClaims, and no longer creates pods. The chart has these RBAC changes.

* **`--method` is removed.** No released chart sets it. If you pass it in your own manifests, remove it, otherwise the controller does not start.
* **PVCs being provisioned during the upgrade** continue: a helper PVC created by v0.x is adopted when it requests the same volume as your PVC.

At start and every 10 minutes the controller runs a migration pass. It changes no data, it only adds metadata and reports what v0.x left behind:

* **Volumes provisioned by v0.x** get the `csi.hybrid.sinextra.dev/managed` label and the `migrated` annotation.
* **Reclaim policy.** v0.x could leave a volume with `Retain` when the hybrid class says `Delete`. Such volumes get a `Warning ReclaimPolicyMismatch` event on the PVC and are counted in `hybrid_reclaim_policy_mismatch_pv_total`. They are only fixed when you enable `fixReclaimPolicy`. A migrated volume is checked until its policy matches once; later changes, for example to `Retain` to keep the data, are left alone.
* **Leaked volumes and helpers.** Backend volumes with `Retain` and no claim, or released from a helper PVC, get a `Warning OrphanedVolume` event (`hybrid_orphaned_pv_total`). Helper PVCs whose PVC is gone get a `Warning OrphanedHelper` event (`hybrid_orphaned_helper_pvc_total`). They are **never deleted automatically**, because the plugin cannot prove it owns them. Check and delete them yourself.
* **Helper pods** of the v0.x `pod` method are deleted once their helper PVC is bound or gone.

To list what the migration found:

```shell
kubectl get events -A --field-selector reason=ReclaimPolicyMismatch
kubectl get events -A --field-selector reason=OrphanedVolume
kubectl get events -A --field-selector reason=OrphanedHelper
```

## Uninstall

The plugin is only needed while volumes are being provisioned. Bound volumes keep working without it.

1. Stop creating PVCs of the hybrid StorageClasses.
2. Wait until no helper PVC is left, so no provisioning is in progress:

   ```shell
   kubectl get pvc -A -l csi.hybrid.sinextra.dev/role=helper
   ```

3. Uninstall the release:

   ```shell
   helm uninstall -n csi-hybrid hybrid-csi-plugin
   ```

If the controller is removed while provisioning is in progress, PVCs and namespaces that are deleted later stay `Terminating` because of the plugin's finalizers. See [stuck PVC or namespace](faq.md#a-pvc-or-namespace-is-stuck-terminating) in the FAQ.
