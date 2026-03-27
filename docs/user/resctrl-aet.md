# Resctrl/AET Energy Attribution

Kepler can use Intel's **Application Energy Telemetry (AET)** hardware to
directly measure per-workload core energy consumption instead of estimating it
from CPU utilization ratios.

> **Status:** Experimental. Enable with `--experimental.resctrl.enabled`.

## What It Does

By default, Kepler attributes node-level RAPL energy to pods using CPU time
ratios. This works well for core-dominated workloads but can be inaccurate when
workloads have different power-per-cycle characteristics (e.g., AVX-512 heavy
vs. memory-bound code).

When resctrl/AET is enabled, Kepler reads hardware core energy counters from the
Linux resctrl filesystem and uses them to improve pod-level energy attribution on
package (CPU socket) zones. Non-package zones like DRAM continue to use the
standard CPU time ratio model.

For implementation details on the hybrid attribution pipeline, see the
[Power Attribution Guide](../developer/power-attribution-guide.md#resctrlaet-attribution).

## Requirements

- **CPU**: Intel Xeon processor with AET support (Clearwater Forest or later)
- **Kernel**: Linux 7.0 or later with `CONFIG_X86_CPU_RESCTRL=y`. AET resctrl
  monitoring was merged in
  [v7.0-rc1](https://github.com/torvalds/linux/commit/a8848c4b43ad00c8a18db080206e3ffa53a08b91).
- **Mount**: resctrl filesystem mounted at `/sys/fs/resctrl`

  ```bash
  mount -t resctrl resctrl /sys/fs/resctrl
  ```

- **AET zones**: AET zone directories (`mon_PERF_PKG_*`) with a `core_energy`
  file must exist under `/sys/fs/resctrl/mon_data/`, and per-group counters
  must appear under
  `/sys/fs/resctrl/mon_groups/<group>/mon_data/mon_PERF_PKG_*/core_energy`.

Verify AET availability:

```bash
ls /sys/fs/resctrl/info/PERF_PKG_MON/mon_features
# Should list: core_energy (and possibly activity)
```

## Configuration

### CLI Flags

| Flag                                  | Default           | Description                                           |
|---------------------------------------|-------------------|-------------------------------------------------------|
| `--experimental.resctrl.enabled`      | `false`           | Enable resctrl/AET energy attribution                 |
| `--experimental.resctrl.base-path`    | `/sys/fs/resctrl` | Path to resctrl filesystem                            |
| `--experimental.resctrl.passive-mode` | `false`           | Discover existing mon_groups instead of creating them |

### Config File (YAML)

```yaml
experimental:
  resctrl:
    enabled: true
    basePath: /sys/fs/resctrl
    passiveMode: false
```

### Operating Modes

**Active mode** (default): Kepler creates a `mon_group` for each running pod,
assigns the pod's PIDs, and reads core energy from AET hardware counters.
Groups are automatically cleaned up when pods terminate.

**Passive mode**: An external orchestrator manages the `mon_groups` (for
example, a custom controller that creates UUID-named groups for each pod).
Kepler discovers groups matching Kubernetes pod UUIDs and reads their energy
counters. This is useful when another component already manages resctrl
resources, or when Kepler runs with a read-only resctrl mount.

## Kubernetes Deployment

### Using Kustomize

Two patches are provided for different operating modes:

**Active mode** — Kepler manages resctrl groups (requires write access):

```yaml
patches:
  - path: patches/enable-resctrl.yaml
```

**Passive mode** — Kepler discovers existing groups (read-only mount):

```yaml
patches:
  - path: patches/enable-resctrl-passive.yaml
```

### Manual Deployment

**Active mode** (read-write mount):

```yaml
spec:
  template:
    spec:
      containers:
        - name: kepler
          args:
            - --experimental.resctrl.enabled
          volumeMounts:
            - name: resctrl
              mountPath: /sys/fs/resctrl
      volumes:
        - name: resctrl
          hostPath:
            path: /sys/fs/resctrl
            type: Directory
```

**Passive mode** (read-only mount):

```yaml
spec:
  template:
    spec:
      containers:
        - name: kepler
          args:
            - --experimental.resctrl.enabled
            - --experimental.resctrl.passive-mode
          volumeMounts:
            - name: resctrl
              mountPath: /sys/fs/resctrl
              readOnly: true
      volumes:
        - name: resctrl
          hostPath:
            path: /sys/fs/resctrl
            type: Directory
```

## Prometheus Metrics

When resctrl is enabled, an additional metric is exported:

### `kepler_pod_resctrl_core_energy_joules_total`

Raw cumulative AET hardware counter — the total core energy measured by AET
for this pod's resctrl monitoring group, summed across all CPU packages. This
is *not* scaled or normalized by the hybrid attribution pipeline.

The attributed pod energy in `kepler_pod_cpu_joules_total` incorporates AET
data internally but also includes uncore and residual shares. The two metrics
are not directly comparable in absolute terms; however, their relative ratios
between pods are consistent.

Only pods with resctrl monitoring groups emit this metric.

| Label           | Description               |
|-----------------|---------------------------|
| `pod_id`        | Kubernetes pod UID        |
| `pod_name`      | Pod name                  |
| `pod_namespace` | Pod namespace             |
| `state`         | `running` or `terminated` |

Example PromQL queries:

```promql
# Core energy rate for resctrl-attributed pods
rate(kepler_pod_resctrl_core_energy_joules_total[5m])

# Compare AET-measured core energy vs total attributed package energy
kepler_pod_resctrl_core_energy_joules_total
  / on(pod_id) kepler_pod_cpu_joules_total{zone="package-0"}
```

## Troubleshooting

**"resctrl/AET init failed"**: AET is not available on this CPU. Check kernel
support and that the resctrl filesystem is mounted.

**Pod missing from `kepler_pod_resctrl_core_energy_joules_total`**: The pod's
`mon_group` could not be created or discovered. In active mode, check that
Kepler has write access to `/sys/fs/resctrl/mon_groups/`. In passive mode,
verify the external orchestrator is creating groups with pod UUID names.

**Energy values seem too low for some pods**: During a pod's first collection
cycle, Kepler seeds the AET baseline without producing a delta. The pod uses
standard CPU-ratio attribution for that cycle and switches to AET-based
attribution starting from the second cycle.

**All pods show ratio-based attribution**: If any pod lacks a resctrl group
or is in its seed cycle, the system operates in mixed mode. Check logs for
`"Failed to read resctrl energy"` or `"Failed to seed resctrl baseline"`
warnings.
