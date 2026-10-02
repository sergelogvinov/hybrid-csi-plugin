# Fast answers to common questions

## Check the plugin

```shell
kubectl get CSIDriver
kubectl get CSINode -ocustom-columns=NODE:.metadata.name,DRV:.spec.drivers
```

## A PVC stays Pending

Look at the events of the PVC first:

```shell
kubectl -n <ns> describe pvc <name>
```

| Event | Meaning |
|-------|---------|
| `BackendSelected` | A backend is chosen and the helper PVC `pvc-<uid of your PVC>` is created. The backend is provisioning |
| `WaitingForBackend` | A warning of the helper, copied to your PVC. Usually the backend's own error: quota, missing driver, no capacity |
| `Rescheduled` | The pod is scheduled again, the message says why: no backend in the list can serve the selected node, the helper was not provisioned within `--helper-timeout`, the backend rejected the node (for example, out of space), or the volume did not fit the node. The helper, if any, is deleted |
| `HelperTimeout` / `BackendRejected` / `VolumeNodeMismatch` | The PVC was rescheduled `--helper-max-reschedules` times already. The plugin keeps waiting and only emits events. Delete the helper PVC to try again |

The helper PVC is in the same namespace as your PVC. Its own events show what the backend is doing:

```shell
kubectl -n <ns> get pvc -l csi.hybrid.sinextra.dev/role=helper
kubectl -n <ns> describe pvc pvc-<uid of your PVC>
```

Common causes:

* The hybrid StorageClass does not use `volumeBindingMode: WaitForFirstConsumer`.
* None of the backends serves the node: check the backend's `allowedTopologies` and that its CSI driver runs on the node (`kubectl get csinode <node>`).
* A ResourceQuota rejects the helper, see [ResourceQuota](install.md#resourcequota).

## A PVC or namespace is stuck Terminating

While a PVC is being provisioned it has the `csi.hybrid.sinextra.dev/provisioning` finalizer, and its helper has `csi.hybrid.sinextra.dev/helper`. The controller removes them when the PVC is deleted, so deletion finishes as long as the controller runs. If it is stopped or uninstalled, start it again, it cleans up by itself.

If you cannot run the controller, remove the finalizers by hand:

```shell
kubectl -n <ns> get pvc -o custom-columns=NAME:.metadata.name,FINALIZERS:.metadata.finalizers
kubectl -n <ns> patch pvc <name> --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]'
```

Removing the finalizer from a helper whose backend StorageClass uses `Retain` may leave its backend volume behind. Check for backend volumes that are not managed by the plugin afterwards, and delete the leftovers:

```shell
kubectl get pv -l 'csi.hybrid.sinextra.dev/managed!=true'
```

## Volume expansion

Expansion is done by the backend: the volume keeps the backend's CSI driver, and the backend's `external-resizer` resizes it. Kubernetes checks only `allowVolumeExpansion` of the hybrid StorageClass, so set it to `true` only if **every** backend in the list allows expansion. Non-CSI backends (in-tree, local-path) cannot expand.

## Snapshots, restore and clone

**Taking a snapshot** of a hybrid PVC works as for any PVC of the backend. The snapshot controller picks the `VolumeSnapshotClass` by the CSI driver of the volume, so either create a default `VolumeSnapshotClass` for every backend driver, or name the backend's class in the `VolumeSnapshot`.

**Restoring or cloning** into a hybrid PVC (`dataSource` / `dataSourceRef`) passes the source to the backend. The backend is still chosen by the node only, not by the driver of the snapshot or source PVC. If the chosen backend cannot use the source, the backend fails, its errors are shown as `WaitingForBackend`, and the PVC is rescheduled after `--helper-timeout`. Restoring across backends is not supported.

For reliable restores, restore into a hybrid StorageClass that lists only the backend of the snapshot, or into the backend StorageClass itself.

## The reclaim policy of a volume differs from its StorageClass

`kubectl get pv` shows the backend StorageClass, because the volume stays a volume of the backend. The reclaim policy is taken from the hybrid StorageClass. A volume of v0.x with a different policy is reported with a `ReclaimPolicyMismatch` event, see [Upgrade from v0.x](install.md#upgrade-from-v0x).

## Which hybrid StorageClass and PVC does a volume belong to

The plugin adds them to the volume:

```shell
kubectl get pv -l csi.hybrid.sinextra.dev/managed=true \
  -o custom-columns='NAME:.metadata.name,CLAIM:.metadata.annotations.csi\.hybrid\.sinextra\.dev/claim,CLASS:.metadata.annotations.csi\.hybrid\.sinextra\.dev/storage-class,BACKEND:.spec.storageClassName'
```
