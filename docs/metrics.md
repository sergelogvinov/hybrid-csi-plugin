# Metrics documentation

This document is a reflection of the current state of the exposed metrics of the Hybrid CSI controller.

## Gather metrics

Enabling the metrics is done by setting the `--http-endpoint` flag to the desired address and port.

```yaml
hybrid-csi-controller --http-endpoint=:8080
```

### Helm chart values

The following values can be set in the Helm chart to expose the metrics of the Talos CCM.

```yaml
controller:
  podAnnotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "8080"
```

## Metrics exposed by the CSI controller
