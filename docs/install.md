# Install plugin

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
