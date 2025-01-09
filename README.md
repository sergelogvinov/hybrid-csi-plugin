# Hybrid CSI Plugin

Status: **Alpha**

The Hybrid CSI Plugin is a Container Storage Interface (CSI) plugin that allows using multiple storage backends in one Kubernetes cluster. This means you can connect different types of storage systems and use them for specific workloads based on their needs.

In Kubernetes, StatefulSets and many Kubernetes Operators usually require a single storage class to work properly. However, in a hybrid environment, you often have different storage backends assigned to different worker groups. If you want to deploy a StatefulSet across these worker groups in the same cluster, this plugin can help you.

## How does the Hybrid CSI Plugin help

The Hybrid CSI Plugin works like a middleware between Kubernetes and your storage backends. It does the following:
* It receives storage requests from Kubernetes.
* It prepares Persistent Volumes (PVs) for the deployment.
* It forwards storage requests to the correct storage backend based on the node group and workload requirements.

This allows Kubernetes to manage storage resources efficiently and flexibly within one cluster, without being restricted to a single storage class.

In short, the Hybrid CSI Plugin simplifies storage management in complex Kubernetes cluster, enabling you to combine multiple storage backends seamlessly and optimize resource usage for different types of workloads.

## In Scope

* [Dynamic provisioning](https://kubernetes-csi.github.io/docs/external-provisioner.html): Volumes are created dynamically when `PersistentVolumeClaim` objects are created.
* [Topology](https://kubernetes-csi.github.io/docs/topology.html): feature to schedule Pod to Node where disk volume pool exists.

## Overview

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
* `storageClasses`: Comma-separated list of storage classes, the order is important. The first storage class has the highest priority.

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

We've deployed a StatefulSet with two pods, each pod has a PVC with a different storage class. The first PVC is bound to a PV created by the `proxmox` storage class, the second PVC is bound to a PV created by the `hcloud-volumes` storage class.

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
