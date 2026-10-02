# Metrics documentation

This document is a reflection of the current state of the exposed metrics of the Hybrid CSI controller.

## Gather metrics

Enabling the metrics is done by setting the `--http-endpoint` flag to the desired address and port.

```yaml
hybrid-csi-controller --http-endpoint=:8080
```

### Helm chart values

The following values expose the metrics of the controller and let Prometheus scrape them.

```yaml
metrics:
  enabled: true
  port: 8080

podAnnotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "8080"
```

## Metrics exposed by the CSI controller

### Provisioning

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hybrid_provision_phase_total` | counter | `phase`, `result` | Provisioning phases by result, see below |
| `hybrid_helper_pvcs` | gauge | | Helper PVCs that exist now, one for every PVC being provisioned |
| `hybrid_helper_age_seconds` | histogram | | Age of helper PVCs when they are deleted: how long the backends take to provision |

Phases of `hybrid_provision_phase_total`:

| `phase` | `result` | Description |
|---------|----------|-------------|
| `backend` | `success`, `error` | Choice of the backend StorageClass. `error`: no backend can serve the node |
| `helper` | `success`, `error` | Creation of the helper PVC |
| `move` | `success`, `error` | Move of the volume to the PVC and binding of the PVC |
| `cleanup` | `success`, `error` | Deletion of the helper and removal of the PVC finalizer |
| `reschedule` | `no-backend`, `helper-timeout`, `backend-rejected`, `node-affinity` | The PVC is rescheduled to another node, `result` is the reason |

A growing `hybrid_helper_pvcs`, or a `reschedule` rate above zero, usually means a backend does not provision. Check the events of the PVCs, see the [FAQ](faq.md#a-pvc-stays-pending).

### Migration from v0.x

These gauges are set by the migration pass, which runs at start and every 10 minutes, see [Upgrade from v0.x](install.md#upgrade-from-v0x). They are expected to be zero.

| Metric | Type | Description |
|--------|------|-------------|
| `hybrid_reclaim_policy_mismatch_pv_total` | gauge | Volumes of v0.x whose reclaim policy differs from their hybrid StorageClass |
| `hybrid_orphaned_pv_total` | gauge | Backend volumes that look leaked by v0.x |
| `hybrid_orphaned_helper_pvc_total` | gauge | Helper PVCs of v0.x whose PVC is gone |

### Provisioner library

The [external provisioner library](https://github.com/kubernetes-sigs/sig-storage-lib-external-provisioner) metrics, with the `controller` subsystem:

| Metric | Type | Labels |
|--------|------|--------|
| `controller_persistentvolumeclaim_provision_total` | counter | `class`, `source` |
| `controller_persistentvolumeclaim_provision_failed_total` | counter | `class`, `source` |
| `controller_persistentvolumeclaim_provision_duration_seconds` | histogram | `class`, `source` |
| `controller_persistentvolume_delete_total` | counter | `class` |
| `controller_persistentvolume_delete_failed_total` | counter | `class` |
| `controller_persistentvolume_delete_duration_seconds` | histogram | `class` |

`provision_failed_total` also counts the passes that wait for the backend, because each of them returns an error to the library.

The Go runtime and process metrics are exposed as well.
