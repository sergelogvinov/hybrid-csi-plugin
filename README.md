# Hybrid CSI Plugin

The Hybrid CSI Plugin is a Container Storage Interface (CSI) plugin that allows using multiple storage backends in one Kubernetes cluster. This means you can connect different types of storage systems and use them for specific workloads based on their needs.

In Kubernetes, StatefulSets and many Kubernetes Operators usually require a single storage class to work properly. However, in a hybrid environment, you often have different storage backends assigned to different worker groups. If you want to deploy a StatefulSet across these worker groups in the same cluster, this plugin can help you.

## How it works

The plugin is a provisioner for a hybrid StorageClass, which lists ordered backend StorageClasses. It never stores data itself. Every volume is created by one of the backends and stays a volume of that backend.

When a pod with a hybrid PVC is scheduled to a node:

1. The plugin picks the first backend in the list that can serve the node. A backend fits if its topology covers the node and its CSI driver runs there.
2. It creates a helper PVC of that backend in the same namespace, for the same node. The backend provisions a volume for the helper as usual.
3. Once the volume exists, the plugin moves it to your PVC and deletes the helper.

The volume keeps the backend's StorageClass and driver, so attach, mount, expansion, snapshots and deletion are handled by the backend's own CSI components. The plugin is only needed while a volume is being provisioned. Bound volumes keep working if it is stopped or uninstalled.

### Guarantees

* **No leaks on crashes.** Every step can be repeated. Finalizers on your PVC and on the helper make sure a crash, a restart, or a PVC or namespace deleted in the middle of provisioning ends with either a bound PVC or a volume reclaimed by its reclaim policy. Volumes created for a helper that are never used are always deleted.
* **The volume is never left unbound.** It is moved to your PVC in a single write, so no other claim can take it in between.
* **The reclaim policy comes from the hybrid StorageClass**, and is set in the same write as the move.
* **A stuck backend does not block the pod.** If the helper is not provisioned within `--helper-timeout` (default `10m`), the PVC is rescheduled so the pod can land on another node. This happens at most `--helper-max-reschedules` times (default `3`).
* **Backend errors show up on your PVC.** Warning events of the helper are mirrored to it as `WaitingForBackend`.

### Limitations

* The hybrid StorageClass must use `volumeBindingMode: WaitForFirstConsumer`: the backend is chosen for the selected node.
* While a volume is being provisioned, both your PVC and the helper count against the namespace's ResourceQuota, see [ResourceQuota](docs/install.md#resourcequota).
* **Expansion** works when the backend supports it: set `allowVolumeExpansion` on the hybrid StorageClass only if every backend allows it. Non-CSI backends (in-tree, local-path) cannot expand.
* **Restore and clone** (`dataSource`/`dataSourceRef`) are passed to the backend, but the backend is not yet chosen by the snapshot's or source PVC's driver. A backend that cannot use the source fails, and the PVC is rescheduled after the helper timeout. See the [FAQ](docs/faq.md#snapshots-restore-and-clone).
* Cloning or restoring across backends is not supported.

How the plugin works inside is explained in [docs/architecture.md](docs/architecture.md).

## In Scope

* [Dynamic provisioning](https://kubernetes-csi.github.io/docs/external-provisioner.html): Volumes are created dynamically when `PersistentVolumeClaim` objects are created.
* [Topology](https://kubernetes-csi.github.io/docs/topology.html): the backend is chosen for the node the pod is scheduled to.

## Overview

The plugin does not require any cloud provider credentials. It only talks to the Kubernetes API, and the backends provision the volumes.

Installation command:

```shell
kubectl create ns csi-hybrid
helm upgrade -i -n csi-hybrid hybrid-csi-plugin oci://ghcr.io/sergelogvinov/charts/hybrid-csi-plugin
```

For details about how to install, configure, upgrade from v0.x and uninstall the plugin, see the [installation instructions](docs/install.md). Metrics are described in [docs/metrics.md](docs/metrics.md).

### Storage Class Definition

Storage Class resource:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: hybrid
parameters:
  storageClasses: proxmox,hcloud-volumes
provisioner: csi.hybrid.sinextra.dev
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
```

Storage parameters:
* `storageClasses`: Comma-separated list of backend storage classes, the order is important. The first storage class that can serve the node is used.

The `reclaimPolicy` of the hybrid class is applied to every volume, whatever the backend class says. Set `allowVolumeExpansion: true` only if every backend allows expansion.

## Deployment examples

Deploy a test statefulSet, it uses the `hybrid` storage class which is defined above.

```shell
kubectl apply -f https://raw.githubusercontent.com/sergelogvinov/hybrid-csi-plugin/refs/heads/main/docs/deploy/test-statefulset.yaml
```

Check status of PV and PVC

```shell
$ kubectl -n default get pods,pvc
NAME         READY   STATUS    RESTARTS   AGE
pod/test-0   1/1     Running   0          31s
pod/test-1   1/1     Running   0          31s

NAME                                   STATUS   VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS   VOLUMEATTRIBUTESCLASS   AGE
persistentvolumeclaim/storage-test-0   Bound    pvc-64440564-75e9-4926-82ef-280f412b11ee   1Gi        RWO            hybrid         <unset>                 32s
persistentvolumeclaim/storage-test-1   Bound    pvc-811cc51e-9c9f-4476-92e1-37382b175e7f   10Gi       RWO            hybrid         <unset>                 32s

$ kubectl -n default get pv pvc-64440564-75e9-4926-82ef-280f412b11ee pvc-811cc51e-9c9f-4476-92e1-37382b175e7f
NAME                                       CAPACITY   ACCESS MODES   RECLAIM POLICY   STATUS   CLAIM                    STORAGECLASS     VOLUMEATTRIBUTESCLASS   REASON   AGE
pvc-64440564-75e9-4926-82ef-280f412b11ee   1Gi        RWO            Delete           Bound    default/storage-test-0   proxmox          <unset>                          84s
pvc-811cc51e-9c9f-4476-92e1-37382b175e7f   10Gi       RWO            Delete           Bound    default/storage-test-1   hcloud-volumes   <unset>                          81s
```

We've deployed a StatefulSet with two pods on different nodes. Both PVCs use the `hybrid` storage class. The first is bound to a PV created by the `proxmox` backend, the second to a PV created by the `hcloud-volumes` backend, because each backend serves a different node.

To see how a PVC was provisioned, check its events:

```shell
$ kubectl -n default describe pvc storage-test-0
Events:
  Type    Reason                 From                     Message
  ----    ------                 ----                     -------
  Normal  BackendSelected        csi.hybrid.sinextra.dev  Backend storage class proxmox is selected for node node-1, helper pvc-64440564-... is created
  Normal  ProvisioningSucceeded  csi.hybrid.sinextra.dev  Successfully provisioned volume pvc-64440564-...
```

## FAQ

See [FAQ](docs/faq.md) for answers to common questions.

## Resources

* https://github.com/kubernetes-sigs/sig-storage-lib-external-provisioner/tree/master
* https://arslan.io/2018/06/21/how-to-write-a-container-storage-interface-csi-plugin/
* https://kubernetes-csi.github.io/docs/

## Contributing

Contributions are welcomed and appreciated!
See [Contributing](CONTRIBUTING.md) for our guidelines.

## License

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
