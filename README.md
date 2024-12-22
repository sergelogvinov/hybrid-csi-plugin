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
* [Storage capacity](https://kubernetes.io/docs/concepts/storage/storage-capacity/): helps to choose the right storage backend based on the storage capacity.

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
