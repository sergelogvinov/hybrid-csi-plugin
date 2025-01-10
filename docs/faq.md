# Fast answers to common questions

## Check the plugin

```shell
kubectl get CSIDriver
kubectl get CSINode -ocustom-columns=NODE:.metadata.name,DRV:.spec.drivers
```
