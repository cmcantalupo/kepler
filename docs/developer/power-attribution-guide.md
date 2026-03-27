# Kepler Power Attribution Guide

This guide explains how Kepler measures and attributes power consumption to
processes, containers, VMs, and pods running on a system.

## Table of Contents

1. [Bird's Eye View](#birds-eye-view)
2. [Key Concepts](#key-concepts)
3. [Attribution Examples](#attribution-examples)
4. [Implementation Overview](#implementation-overview)
5. [Code Reference](#code-reference)
6. [Resctrl/AET Attribution](#resctrlaet-attribution)
7. [Limitations and Considerations](#limitations-and-considerations)

## Bird's Eye View

### The Big Picture

Modern systems lack per-workload energy metering, providing only aggregate
power consumption at the hardware level. Kepler addresses this attribution
challenge through proportional distribution based on resource utilization:

1. **Hardware Energy Collection** - Intel RAPL sensors provide cumulative
   energy counters at package, core, DRAM, and uncore levels
2. **System Activity Analysis** - CPU utilization metrics from `/proc/stat`
   determine the ratio of active vs idle system operation
3. **Power Domain Separation** - Total energy is split into active power
   (proportional to workload activity) and idle power (baseline consumption)
4. **Proportional Attribution** - Active power is distributed to workloads
   based on their CPU time consumption ratios

### Core Philosophy

Kepler implements a **CPU-time-proportional energy attribution model** that
distributes hardware-measured energy consumption to individual workloads based
on their computational resource usage patterns.

The fundamental principle recognizes that system power consumption has two
distinct components:

- **Active Power**: Energy consumed by computational work, proportional to CPU
  utilization and scalable with workload activity
- **Idle Power**: Fixed baseline energy for maintaining system operation,
  including memory refresh, clock distribution, and idle core power states

### Attribution Formula

All workload types use the same proportional attribution formula:

```text
Workload Power = (Workload CPU Time Δ / Node CPU Time Δ) × Active Power
```

This ensures energy conservation - the sum of attributed power remains
proportional to measured hardware consumption while maintaining fairness based
on actual resource utilization.

![Power Attribution Diagram](assets/power-attribution.png)

*Figure 1: Power attribution flow showing how total measured power is
decomposed into active and idle components, with active power distributed
proportionally based on workload CPU time deltas.*

### Multi-Socket and Zone Aggregation

Modern server systems often feature multiple CPU sockets, each with their own
RAPL energy domains. Kepler handles this complexity through zone aggregation:

**Zone Types and Hierarchy:**

- **Package zones**: CPU socket-level energy (e.g., `package-0`, `package-1`)
- **Core zones**: Individual CPU core energy within each package
- **DRAM zones**: Memory controller energy per socket
- **Uncore zones**: Integrated GPU, cache, and interconnect energy
- **PSys zones**: Platform-level energy (most comprehensive when available)

**Aggregation Strategy:**

```text
Total Package Energy = Σ(Package-0 + Package-1 + ... + Package-N)
Total DRAM Energy = Σ(DRAM-0 + DRAM-1 + ... + DRAM-N)
```

**Counter Wraparound Handling:**
Each zone maintains independent energy counters that can wrap around at
different rates. The `AggregatedZone` implementation:

- Tracks last readings per individual zone
- Calculates deltas across wraparound boundaries using `MaxEnergy` values
- Aggregates deltas to provide system-wide energy consumption
- Maintains a unified counter that wraps at the combined `MaxEnergy` boundary

This ensures accurate energy accounting across heterogeneous multi-socket
systems while preserving the precision needed for power attribution
calculations.

## Key Concepts

### CPU Time Hierarchy

CPU time is calculated hierarchically for different workload types:

```text
Process CPU Time = Individual process CPU time from /proc/<pid>/stat
Container CPU Time = Σ(CPU time of all processes in the container)
Pod CPU Time = Σ(CPU time of all containers in the pod)
VM CPU Time = CPU time of the hypervisor process (e.g., QEMU/KVM)
Node CPU Time = Σ(All process CPU time deltas)
```

### Energy vs Power

- **Energy**: Measured in microjoules (μJ) as cumulative counters from hardware
- **Power**: Calculated as rate in microwatts (μW) using `Power = ΔEnergy / Δtime`

### Energy Zones

Hardware energy is read from different zones:

- **Package**: CPU package-level energy consumption
- **Core**: Individual CPU core energy
- **DRAM**: Memory subsystem energy
- **Uncore**: Integrated graphics and other uncore components
- **PSys**: Platform-level energy (most comprehensive when available)

### Independent Attribution

Each workload type (process, container, VM, pod) calculates power
**independently** based on its own CPU time usage. This means:

- Containers don't inherit power from their processes
- Pods don't inherit power from their containers
- VMs don't inherit power from their processes
- Each calculates directly from node active power

## Attribution Examples

### Example 1: Basic Power Split

**System State:**

- Hardware reports: 40W total system power
- Node CPU usage: 25% utilization ratio
- Power split: 40W × 25% = 10W active, 30W idle

**Workload Attribution:**
If a container used 20% of total node CPU time during the measurement
interval:

- **Container power** = (20% CPU usage) × 10W active = 2W

### Example 2: Multi-Workload Scenario

**System State:**

- Total power: 60W
- CPU usage ratio: 33.3% (1/3)
- Active power: 20W, Idle power: 40W
- Node total CPU time: 1000ms

**Process-Level CPU Usage:**

- Process 1 (standalone): 100ms CPU time
- Process 2 (in container-A): 80ms CPU time
- Process 3 (in container-A): 70ms CPU time
- Process 4 (in container-B): 60ms CPU time
- Process 5 (QEMU hypervisor): 200ms CPU time
- Process 6 (in container-C, pod-X): 90ms CPU time
- Process 7 (in container-D, pod-X): 110ms CPU time

**Hierarchical CPU Time Aggregation:**

- Container-A CPU time: 80ms + 70ms = 150ms
- Container-B CPU time: 60ms
- Container-C CPU time: 90ms (part of pod-X)
- Container-D CPU time: 110ms (part of pod-X)
- Pod-X CPU time: 90ms + 110ms = 200ms
- VM CPU time: 200ms (QEMU hypervisor process)

**Independent Power Attribution (each from node active power):**

- Process 1: (100ms / 1000ms) × 20W = 2W
- Process 2: (80ms / 1000ms) × 20W = 1.6W
- Process 3: (70ms / 1000ms) × 20W = 1.4W
- Process 4: (60ms / 1000ms) × 20W = 1.2W
- Process 5: (200ms / 1000ms) × 20W = 4W
- Process 6: (90ms / 1000ms) × 20W = 1.8W
- Process 7: (110ms / 1000ms) × 20W = 2.2W
- Container-A: (150ms / 1000ms) × 20W = 3W
- Container-B: (60ms / 1000ms) × 20W = 1.2W
- Container-C: (90ms / 1000ms) × 20W = 1.8W
- Container-D: (110ms / 1000ms) × 20W = 2.2W
- Pod-X: (200ms / 1000ms) × 20W = 4W
- VM: (200ms / 1000ms) × 20W = 4W

**Note:** Each workload type calculates power independently from node active power based on its own CPU time, not by inheriting from constituent workloads.

### Example 3: Container with Multiple Processes

**Container "web-server":**

- Process 1 (nginx): 100ms CPU time
- Process 2 (worker): 50ms CPU time
- Container total: 150ms CPU time

**If node total CPU time is 1000ms:**

- Container CPU ratio: 150ms / 1000ms = 15%
- Container power: 15% × active power

### Example 4: Pod with Multiple Containers

**Pod "frontend":**

- Container 1 (nginx): 200ms CPU time
- Container 2 (sidecar): 50ms CPU time
- Pod total: 250ms CPU time

**If node total CPU time is 1000ms:**

- Pod CPU ratio: 250ms / 1000ms = 25%
- Pod power: 25% × active power

### Example 5: Hyperthreading and the CPU-Ratio Blind Spot

This example illustrates a structural limitation of CPU-time ratio
attribution on hyperthreaded systems.

**System:** Intel Xeon, 128 physical cores, HT enabled → 256 logical
CPUs. Single pod running DGEMM (compute-intensive, saturates all
physical cores).

DGEMM is ALU-bound and cannot benefit from hyperthreading. Each physical
core is 100% busy, but the HT sibling sits idle. The processor draws
close to TDP, yet the OS reports ~50% CPU utilization (128 busy / 256
logical CPUs).

```text
RAPL total power:     300 W
Node CPU usage ratio: 0.50  (128 busy / 256 logical CPUs)
Active power:         150 W
Idle power:           150 W
Pod power:            1.0 × 150 W = 150 W
```

Half the pod's real power is misclassified as idle and left
unattributed. This is a structural artifact: the ratio model cannot
distinguish "physical cores saturated with HT siblings idle" from
"system genuinely 50% idle."

With resctrl/AET, the `core_energy` counter reflects actual core power
regardless of logical CPU accounting, sidestepping this error for the
core energy component.

### Example 6: Three-Pod Resctrl/AET Comparison

This example shows how the same three pods receive different power
attributions depending on whether Kepler tracks them with hardware AET
measurements or falls back to CPU-time ratio estimation. The system state
is identical across all three cases — only the level of resctrl coverage
changes.

**Common System State (1-second measurement interval):**

- RAPL package-0 energy delta: 100 J → 100 W total
- Node CPU usage ratio: 0.50
- Active energy: 50 J, Idle energy: 50 J
- Node total CPU time delta: 1000 ms

**Three Pods:**

| Pod              | Workload             | CPU Time | CPU Ratio | AET Core Energy |
|------------------|----------------------|----------|-----------|-----------------|
| A (web-app)      | HTTP serving         | 200 ms   | 0.20      | 20 J            |
| B (ml-inference) | AVX-heavy model eval | 500 ms   | 0.50      | 40 J            |
| C (log-shipper)  | Lightweight sidecar  | 100 ms   | 0.10      | 10 J            |
| *Other system*   | Kernel threads, etc. | 200 ms   | 0.20      | —               |

Pod B's AET core energy (40 J) is disproportionately high relative to its
CPU time because AVX-512 inference consumes significantly more power per
cycle than scalar code. Pod C's core energy (10 J) is proportionally low —
a mostly-idle sidecar that wakes briefly to flush buffers.

#### Case 1: No Pods Tracked by Resctrl

All pods use the standard CPU-time ratio model. Each pod receives a share
of **active energy only** — idle energy is not attributed to any pod.

```text
Pod Power = cpuTimeRatio × ActivePower
```

| Pod   | Calculation | Power    |
|-------|-------------|----------|
| A     | 0.20 × 50 W | **10 W** |
| B     | 0.50 × 50 W | **25 W** |
| C     | 0.10 × 50 W | **5 W**  |
| *Sum* |             | *40 W*   |

Total attributed to pods: 40 W of 50 W active. The remaining 10 W of
active energy covers non-pod system activity. The 50 W idle energy is
not distributed.

**Observation:** Pod B (ml-inference) and a hypothetical scalar workload
with the same CPU time would receive identical power — the ratio model
cannot distinguish instruction-level power differences.

#### Case 2: Pods A and B Tracked, Pod C Not Tracked

Pods A and B have resctrl monitoring groups with AET data. Pod C does
not. Kepler selects **mixed mode**, decomposing RAPL into three pools
using the root-level AET core_energy counter.

Assume the root-level `core_energy` delta (total system core) is 80 J.
The tracked mon_group deltas sum to 60 J (Pod A: 20 J, Pod B: 40 J).

**Step 1 — Decompose RAPL into three pools:**

```text
tracked_core   = 20 + 40 = 60 J     (sum of mon_group core deltas)
untracked_core = 80 − 60 = 20 J     (rootCore − tracked → non-AET pods)
uncore         = 100 − 80 = 20 J    (RAPL − rootCore → all pods)
normFactor     = min(1.0, 80/60) = 1.0
```

**Step 2 — Attribute per pod:**

Resctrl pods receive tracked core + uncore share. Non-AET pods receive
untracked core share + uncore share. The untracked core denominator is
total non-AET CPU time (non-AET pods + system processes = 1000 − 700 =
300 ms), so system threads get their implicit share without it being
attributed to pods.

| Pod              | Tracked Core    | Untracked Core           | Uncore Share     | Total   | Power      |
|------------------|-----------------|--------------------------|------------------|---------|------------|
| A *(resctrl)*    | 20 × 1.0 = 20 J | —                        | 20 × 0.20 = 4 J  | 24 J    | **24 W**   |
| B *(resctrl)*    | 40 × 1.0 = 40 J | —                        | 20 × 0.50 = 10 J | 50 J    | **50 W**   |
| C *(ratio-only)* | —               | 20 × (100/300) = 6.67 J  | 20 × 0.10 = 2 J  | 8.67 J  | **8.67 W** |
| *Other system*   | —               | 20 × (200/300) = 13.33 J | 20 × 0.20 = 4 J  | 17.33 J | *17.33 W*  |
| *Sum*            | 60 J            | 20 J                     | 20 J             | 100 J   | *100 W*    |

Energy conservation holds: tracked core (60 J) + untracked core (20 J) +
uncore (20 J) = 100 J = RAPL delta. "Other system" shows the energy
implicitly consumed by non-pod processes — this energy is not attributed
to any pod.

#### Case 3: All Three Pods Tracked by Resctrl

Every running pod has a resctrl monitoring group with valid AET deltas.
Kepler selects **all-resctrl mode**, which operates against the **full
RAPL delta** (100 J) rather than just active energy. The residual now
encompasses true uncore energy, idle power, and non-pod system activity.

**Step 1 — Sum raw AET core deltas (no scaling):**

```text
total_core = 20 + 40 + 10 = 70 J
```

**Step 2 — Compute residual and normalization factor:**

```text
residual   = max(0, RAPL_delta − total_core) = max(0, 100 − 70) = 30 J
normFactor = min(1.0, RAPL_delta / total_core) = min(1.0, 100/70) = 1.0
```

**Step 3 — Attribute per pod:**

| Pod   | Core (raw)      | Residual Share   | Total | Power    |
|-------|-----------------|------------------|-------|----------|
| A     | 20 × 1.0 = 20 J | 30 × 0.20 = 6 J  | 26 J  | **26 W** |
| B     | 40 × 1.0 = 40 J | 30 × 0.50 = 15 J | 55 J  | **55 W** |
| C     | 10 × 1.0 = 10 J | 30 × 0.10 = 3 J  | 13 J  | **13 W** |
| *Sum* |                 |                  |       | *94 W*   |

The remaining 6 J of residual (30 × 0.20) covers non-pod system
processes. Total: 94 + 6 = 100 J = full RAPL budget. ✓

#### Summary Comparison

| Pod              | Case 1 (ratio) | Case 2 (mixed)    | Case 3 (all-resctrl) |
|------------------|----------------|-------------------|----------------------|
| A (web-app)      | 10 W           | 24 W              | 26 W                 |
| B (ml-inference) | 25 W           | 50 W              | 55 W                 |
| C (log-shipper)  | 5 W            | 8.67 W            | 13 W                 |
| **Pod total**    | **40 W**       | **82.67 W**       | **94 W**             |
| Energy budget    | Active (50 W)  | Full RAPL (100 W) | Full RAPL (100 W)    |

**Key takeaways:**

1. **AET captures instruction-level power differences.** Pod B's AVX-heavy
   workload consumes more core energy per cycle than scalar code; the
   ratio model cannot distinguish them.

2. **All-resctrl and mixed modes operate against the full RAPL budget**
   rather than just active energy. Energy conservation holds in every
   case — tracked core + untracked core + uncore = RAPL delta.

3. **The magnitude gap between Case 1 and Cases 2–3 is driven by the
   energy budget, not just by AET precision.** Case 1 distributes only
   Active energy (50 W), while AET modes distribute the full RAPL delta
   (100 W). This 2× budget difference is a direct consequence of the
   example's 50% CPU utilization ratio — at that level, the ratio model
   classifies half of RAPL as idle and leaves it unattributed. At higher
   utilization the gap narrows (e.g., at 80% utilization, Active = 80 W
   and the ratio would be ~1.25×); at lower utilization it widens. Real
   Kubernetes clusters commonly operate between 30–60% utilization, so a
   substantial gap is realistic in production.

## Implementation Overview

### Architecture Flow

```text
Hardware (RAPL) → Device Layer → Monitor (Attribution) → Exporters
    ↑                                ↑
/proc filesystem → Resource Layer ----┘
```

### Core Components

1. **Device Layer** (`internal/device/`): Reads energy from hardware sensors
2. **Resource Layer** (`internal/resource/`): Tracks processes/containers/VMs/ pods
   and calculates CPU time
3. **Monitor Layer** (`internal/monitor/`): Performs power attribution calculations
4. **Export Layer** (`internal/exporter/`): Exposes metrics via Prometheus/stdout

### Attribution Process

1. **Hardware Reading**: Device layer reads total energy from RAPL sensors
2. **CPU Time Calculation**: Resource layer aggregates CPU time hierarchically
3. **Node Power Split**: Monitor calculates active vs idle power based on CPU usage ratio
4. **Workload Attribution**: Each workload gets power proportional to its CPU time
5. **Energy Accumulation**: Energy totals accumulate over time for each workload
6. **Export**: Metrics are exposed for consumption

### Thread Safety

- **Device Layer**: Not required to be thread-safe (single monitor goroutine)
- **Resource Layer**: Not required to be thread-safe
- **Monitor Layer**: All public methods except `Init()` must be thread-safe
- **Singleflight Pattern**: Prevents redundant power calculations during
  concurrent requests

## Code Reference

### Key Files and Functions

#### Node Power Calculation

**File**: `internal/monitor/node.go`
**Function**: `calculateNodePower()`

```go
// Splits total hardware energy into active and idle components
activeEnergy = Energy(float64(deltaEnergy) * nodeCPUUsageRatio)
idleEnergy := deltaEnergy - activeEnergy
```

#### Process Power Attribution

**File**: `internal/monitor/process.go`
**Function**: `calculateProcessPower()`

```go
// Each process gets power proportional to its CPU usage
cpuTimeRatio := proc.CPUTimeDelta / nodeCPUTimeDelta
process.Power = Power(cpuTimeRatio * nodeZoneUsage.ActivePower.MicroWatts())
```

#### Container Power Attribution

**File**: `internal/monitor/container.go`
**Function**: `calculateContainerPower()`

```go
// Container CPU time = sum of all its processes
cpuTimeRatio := container.CPUTimeDelta / nodeCPUTimeDelta
container.Power = Power(cpuTimeRatio * nodeZoneUsage.ActivePower.MicroWatts())
```

#### VM Power Attribution

**File**: `internal/monitor/vm.go`
**Function**: `calculateVMPower()`

```go
// VM CPU time = hypervisor process CPU time
cpuTimeRatio := vm.CPUTimeDelta / nodeCPUTimeDelta
vm.Power = Power(cpuTimeRatio * nodeZoneUsage.ActivePower.MicroWatts())
```

#### Pod Power Attribution

**File**: `internal/monitor/pod.go`
**Function**: `calculatePodPower()`

```go
// Pod CPU time = sum of all its containers
cpuTimeRatio := pod.CPUTimeDelta / nodeCPUTimeDelta
pod.Power = Power(cpuTimeRatio * float64(nodeZoneUsage.ActivePower))
```

#### CPU Time Aggregation

**File**: `internal/resource/informer.go`
**Functions**: `updateContainerCache()`, `updatePodCache()`,
`updateVMCache()`

```go
// Container: Sum of process CPU times
cached.CPUTimeDelta += proc.CPUTimeDelta

// Pod: Sum of container CPU times
cached.CPUTimeDelta += container.CPUTimeDelta

// VM: Direct from hypervisor process
cached.CPUTimeDelta = proc.CPUTimeDelta
```

#### Wraparound Handling

**File**: `internal/monitor/node.go`
**Function**: `calculateEnergyDelta()`

```go
// Handles RAPL counter wraparound
func calculateEnergyDelta(current, previous, maxJoules Energy) Energy {
    if current >= previous {
        return current - previous
    }
    // Handle counter wraparound
    if maxJoules > 0 {
        return (maxJoules - previous) + current
    }
    return 0
}
```

### Data Structures

**File**: `internal/monitor/types.go`

```go
type Usage struct {
    Power       Power  // Current power consumption
    EnergyTotal Energy // Cumulative energy over time
}

type NodeUsage struct {
    EnergyTotal       Energy // Total absolute energy
    ActiveEnergyTotal Energy // Cumulative active energy
    IdleEnergyTotal   Energy // Cumulative idle energy
    Power             Power  // Total power
    ActivePower       Power  // Active power
    IdlePower         Power  // Idle power
}
```

## Resctrl/AET Attribution

> **User guide**: See [docs/user/resctrl-aet.md](../user/resctrl-aet.md) for
> configuration, deployment, and metrics.

### Overview

On Intel Xeon processors with Application Energy Telemetry (AET) support,
Kepler can read per-monitoring-group core energy counters from the Linux resctrl
filesystem instead of estimating core energy from CPU time ratios. This is an
experimental feature enabled via `--experimental.resctrl.enabled`.

AET counters are exposed at two levels in the resctrl filesystem:

**Per-group** (for pods with monitoring groups):

```text
/sys/fs/resctrl/mon_groups/<group>/mon_data/mon_PERF_PKG_*/core_energy
```

**Root-level** (system-wide total, all processes regardless of group membership):

```text
/sys/fs/resctrl/mon_data/mon_PERF_PKG_*/core_energy
```

Each `mon_PERF_PKG_*` directory corresponds to a CPU package (socket). The
per-group `core_energy` file contains a cumulative Joule counter of core energy
consumed by the processes assigned to that monitoring group. The root-level
`core_energy` counter tracks all core energy on the package, including processes
not assigned to any monitoring group. The three-pool mixed attribution model
uses both levels: per-group counters for tracked pods, and the root-level
counter to derive untracked core energy.

### Three-Phase Attribution Pipeline

When resctrl is enabled, pod power attribution in `calculatePodPower()` follows
a three-phase pipeline instead of the single ratio-model pass:

#### Phase 1 — Read Deltas (`resctrlReadDeltas`)

**File**: `internal/monitor/resctrl.go`

For each pod with a resctrl monitoring group:

1. Read current cumulative AET counter per package
2. Compute delta against previous snapshot's baseline
3. Handle counter wraparound (negative delta → treat as zero)
4. **Seed-only detection**: A pod's first successful AET read is
   baseline-only — no delta is produced. The pod uses standard ratio
   attribution for that cycle and contributes meaningful deltas starting
   from the next cycle.

The result is `coreDelta[podID][pkgIdx]` in raw Joules.

```go
// Seed detection: only contribute deltas when a previous baseline exists
if hasPrevBaseline {
    ra.coreDelta[id] = podDeltas
    for pkgIdx, d := range podDeltas {
        ra.totalCoreByPkg[pkgIdx] += d
    }
}
```

The `allPodsTracked` flag is set when:

- A previous snapshot exists (`prev != nil`)
- Every running pod has a resctrl group
- Every tracked pod contributed deltas (had a valid baseline)

#### Phase 2 — Compute Budget (`resctrlComputeBudget`)

**File**: `internal/monitor/resctrl.go`

Two modes are selected automatically based on `allPodsTracked`:

**All-resctrl mode** (`computeBudgetAllResctrl`):

When every pod has AET data, raw deltas are used against the total RAPL
`deltaEnergy` (not scaled by `UsageRatio`):

```text
residual = max(0, RAPL_deltaEnergy − sum(raw_AET_core))
normFactor = min(1.0, RAPL_deltaEnergy / sum(raw_AET_core))
```

Because RAPL and AET counters are read at slightly different times (non-atomic
reads), the difference `RAPL − sum(AET)` can be negative — AET may report
more core energy than RAPL reports total energy. This is physically impossible
but occurs as a measurement timing artifact. The `max(0, ...)` and
`min(1.0, ...)` clamps handle both cases:

- **Case A: RAPL > sum(AET)** — the normal case. There is real residual energy
  (uncore, idle, kernel threads). `normFactor = 1.0` (AET values are used
  as-is), and the positive residual is distributed to all pods by CPU time
  ratio.

- **Case B: sum(AET) > RAPL** — timing artifact. `normFactor = RAPL / sum(AET)`
  (less than 1.0), which scales every pod's AET core delta down so the total
  equals the RAPL budget. `residual = 0` — no uncore budget is allocated
  because all available energy is accounted for as core energy.

In Case A, the residual includes true uncore energy, idle energy, and energy
from non-pod system processes (kernel threads, etc.). Because it is typically
a small fraction of total package energy, the `cpuTimeRatio` split of the
residual has minimal accuracy impact.

**Mixed mode** (`computeBudgetMixed`):

When some pods lack resctrl data, the root-level AET `core_energy` counter
is used to decompose RAPL into three disjoint energy pools:

```text
uncore        = max(0, RAPL_delta − rootCoreDelta)
untrackedCore = max(0, rootCoreDelta − sum(mon_group_core_deltas))
normFactor    = min(1.0, rootCoreDelta / sum(mon_group_core_deltas))
```

As with all-resctrl mode, the `max(0, ...)` and `min(1.0, ...)` clamps handle
timing artifacts from non-atomic reads of RAPL and AET counters. The same
Case A / Case B logic applies at each decomposition boundary.

| Pool           | Source                               | Recipients                                             |
|----------------|--------------------------------------|--------------------------------------------------------|
| Tracked core   | Each mon_group's `core_energy` delta | Directly to that AET pod                               |
| Untracked core | `rootCore − Σ monGroupCore`          | Non-AET pods (by relative CPU time among non-AET pods) |
| Uncore         | `RAPL − rootCore`                    | ALL pods (by `cpuTimeRatio`)                           |

The three pools sum to exactly the RAPL delta, guaranteeing energy
conservation. When `rootCoreDelta` is unavailable (first cycle), the
fallback is a two-pool split using `RAPL − Σ monGroupCore`.

**Normalization**: In both modes, the Case A / Case B clamping logic described
above ensures that `normFactor` scales AET values to fit within the RAPL
budget while preserving relative proportions between pods.

#### Phase 3 — Attribute (`calculatePodPower` loop)

**File**: `internal/monitor/pod.go`

For each pod × zone combination, one of three branches executes:

| Condition                           | Branch                  | Energy Formula                                                  |
|-------------------------------------|-------------------------|-----------------------------------------------------------------|
| Pod has AET deltas + package zone   | Resctrl core            | `normCore + uncore × cpuTimeRatio`                              |
| Pod lacks AET deltas + package zone | Untracked core + uncore | `untrackedCore × (podCPU / nonAET_CPU) + uncore × cpuTimeRatio` |
| Non-package zone or no resctrl      | Pure ratio              | `activeEnergy × cpuTimeRatio`                                   |

```go
// Resctrl pod on package zone:
normCore := coreDeltaJoules * ra.coreNormFactor[zone]
uncoreShareJoules := ra.uncoreEnergy[zone] * cpuTimeRatio
activeEnergy = Energy((normCore + uncoreShareJoules) * float64(Joule))

// Non-resctrl pod on package zone (mixed mode):
// Gets untracked core share + uncore share
untrackedShare := ra.untrackedCore[zone] * (p.CPUTimeDelta / nonAETCPUTotal)
uncoreShare := ra.uncoreEnergy[zone] * cpuTimeRatio
activeEnergy = Energy((untrackedShare + uncoreShare) * float64(Joule))

// Non-package zone or no resctrl data:
activeEnergy = Energy(float64(nodeZoneUsage.activeEnergy) * cpuTimeRatio)
```

Non-resctrl pods receive untracked core energy (the core energy not
attributed to any monitoring group) plus their share of uncore energy.
This ensures energy conservation — tracked core + untracked core + uncore
equals the RAPL delta.

### Attribution Source Tracking

Each pod carries an `AttributionSource` field:

- `AttributionRatio` — energy computed from CPU time ratios (default, and
  used during seed-only cycles even for pods with resctrl groups)
- `AttributionResctrl` — energy computed using AET hardware measurements
  (set only when the pod contributed meaningful AET deltas)

### Multi-Package and Aggregated Zones

RAPL zones can be per-package (`package-0`, `package-1`) or aggregated
(`package`). The implementation handles both:

- **Per-package zones**: Match AET data by package index extracted from
  the zone name via `raplZonePackageIndex()`
- **Aggregated zones**: Sum AET data across all packages

Similarly, AET zone names (`mon_PERF_PKG_0`) are mapped to package indices
via `aetZonePackageIndex()`.

### Resctrl Group Lifecycle

**File**: `internal/monitor/resctrl.go` (management functions)

- **Active mode**: `syncResctrlGroups()` creates groups for new pods, adds
  PIDs, and deletes groups for terminated pods. Stale groups that fail to
  delete are retained for retry on the next cycle.
- **Passive mode**: `discoverResctrlGroups()` lists existing `mon_groups`
  and matches names against running pod UUIDs.

### Key Data Structures

```go
type resctrlAttribution struct {
    coreDelta          map[string]map[int]float64  // [podID][pkgIdx] → raw Joules delta
    totalCoreByPkg     map[int]float64             // [pkgIdx] → sum across pods
    rootCoreDelta      map[int]float64             // [pkgIdx] → root core energy delta
    rootCoreCumulative map[int]float64             // [pkgIdx] → cumulative (for baseline)
    uncoreEnergy       map[EnergyZone]float64      // [zone] → RAPL − rootCore (Joules)
    coreNormFactor     map[EnergyZone]float64      // [zone] → conservation factor
    untrackedCore      map[EnergyZone]float64      // [zone] → rootCore − Σ monGroupCore
    allPodsTracked     bool                        // all-resctrl mode flag
    pods               map[string]*Pod             // pre-built pod entries
}
```

The `Pod` struct carries resctrl state:

- `ResctrlCoreEnergyByPkg map[int]float64` — cumulative AET counter per package
- `AttributionSource` — `AttributionRatio` or `AttributionResctrl`

The `Node` struct carries the root core baseline:

- `AETCoreBaseline map[int]float64` — cumulative root `core_energy` per package

### Interaction with Existing Attribution

The resctrl pipeline is additive — it only affects package zones for pods
with resctrl data. All other attribution (processes, containers, VMs,
non-package pod zones) continues to use the standard CPU-time ratio model
unchanged.

## Limitations and Considerations

### CPU Power States and Attribution Accuracy

Modern CPUs implement sophisticated power management that affects attribution
accuracy beyond simple CPU time percentages:

#### C-States (CPU Sleep States)

- **C0 (Active)**: CPU executing instructions
- **C1 (Halt)**: CPU stopped but cache coherent
- **C2-C6+ (Deep Sleep)**: Progressively deeper sleep states, drawing
  near-zero power

Cores in deep C-states accumulate idle ticks in `/proc/stat`, which
inflates the denominator of `CPUUsageRatio` (active / total). This
reduces the active energy budget and shifts energy into the unattributed
idle bucket — even though the sleeping cores draw negligible power.

In practice the effect is modest: the Linux scheduler spreads work across
cores and constantly wakes idle cores for timer interrupts, kernel
threads, and kubelet housekeeping. C-state transitions occur at
microsecond granularity, so over Kepler's collection interval they
average out. The per-pod `cpuTimeRatio` (busy time / total busy time
across all processes) is unaffected because sleeping cores contribute
zero busy time to both numerator and denominator.

#### P-States (Performance States)

- **Dynamic Frequency Scaling**: CPU frequency adjusts based on workload
- **Voltage Scaling**: Power scales quadratically with voltage changes
- **Turbo Boost**: Short bursts of higher frequency
- **Impact**: High-frequency workloads consume disproportionately more power
  per CPU cycle

### Workload-Specific Characteristics

#### Compute vs Memory-Bound Workloads

```text
Example Scenario:
- Process A: 50% CPU, compute-intensive (high frequency, active execution)
- Process B: 50% CPU, memory-bound (frequent stalls, lower frequency)

Current Attribution: Both receive equal power
Reality: Process A likely consumes 2-3x more power
```

#### Instruction-Level Variations

- **Vector Instructions (AVX/SSE)**: 2-4x more power than scalar operations
- **Execution Units**: Different power profiles for integer vs floating point
- **Cache Behavior**: Cache misses trigger higher memory controller power

### Beyond CPU Attribution

#### Memory Subsystem

- **DRAM Power**: Memory-intensive workloads consume more DRAM power
- **Memory Controller**: Higher bandwidth increases uncore power
- **Cache Hierarchy**: Different access patterns affect cache power

#### I/O and Peripherals

- **Storage I/O**: Triggers storage controller and device power
- **Network I/O**: Consumes network interface and PCIe power
- **GPU Workloads**: Integrated graphics power not captured by CPU metrics

### Temporal Distribution Issues

#### Bursty vs Steady Workloads

```text
10-Second Window Example:
- Process A: Steady 20% CPU throughout interval
- Process B: 100% CPU for 2 seconds, idle for 8 seconds (20% average)

Current Attribution: Both receive equal power
Reality: Process B likely consumed more during its burst due to:
- Higher frequency scaling
- Thermal effects
- Different sleep state behavior
```

### When CPU Attribution Works Well

- **CPU-bound workloads** with similar instruction mixes
- **Steady-state workloads** without significant frequency scaling
- **Relative comparisons** between similar workload types
- **Trend analysis** over longer time periods

### How Resctrl/AET Improves Accuracy

On supported hardware, the [resctrl/AET attribution](#resctrlaet-attribution)
path replaces CPU-time-based core energy estimates with hardware-measured
per-workload core energy, capturing instruction-mix and frequency-scaling
differences. Non-core energy (uncore, idle, system overhead) is still
approximated via CPU time ratios.

### When to Exercise Caution

- **Mixed workload environments** with varying compute vs I/O patterns
- **High-performance computing** workloads using specialized instructions
- **Absolute power budgeting** decisions based solely on Kepler metrics
- **Fine-grained optimization** requiring precise per-process power
  measurement

### Key Metrics

- `kepler_node_cpu_watts{}`: Total node power consumption
- `kepler_node_resctrl_root_core_energy_delta_joules{package}`: Root-level
  resctrl core energy delta per package (total socket core energy from AET;
  only present when resctrl is enabled)
- `kepler_process_cpu_watts{}`: Individual process power
- `kepler_container_cpu_watts{}`: Container-level power
- `kepler_vm_cpu_watts{}`: Virtual machine power
- `kepler_pod_cpu_watts{attribution_source}`: Kubernetes pod power. The
  `attribution_source` label is `"resctrl"` for AET-attributed pods or
  `"ratio"` for CPU-time-ratio-attributed pods.
- `kepler_pod_resctrl_core_energy_joules_total{}`: Raw AET core energy
  (experimental, only on supported hardware — see
  [Resctrl/AET Attribution](#resctrlaet-attribution))

### Resctrl/AET Considerations

#### Metric Invariant Changes

Without resctrl, Kepler maintains these invariants:

- `Σ(process energy) ≈ node activeEnergy`
- `Σ(container energy) ≈ Σ(process energy in container)`
- `Σ(pod energy) ≈ Σ(container energy in pod)`

When resctrl is enabled, **pod-level metrics use AET hardware measurements
while process and container metrics continue to use CPU-time ratios**.
This means:

- `Σ(pod energy)` may differ from `Σ(process energy)` and
  `Σ(container energy)` because pods use hardware-measured core energy
  while processes/containers use estimated energy.
- Pods with `attribution_source="resctrl"` receive hardware-measured core
  energy plus a CPU-time-ratio share of uncore energy. This is more
  accurate than the pure ratio model but uses a fundamentally different
  calculation.

**Recommendation**: When resctrl is enabled, rely on pod-level metrics for
power analysis. Consider disabling process and container metrics
(`metricsLevel: pod`) to avoid confusion from inconsistent invariants.

#### Tracked Pod Energy Model

For pods with AET data (`attribution_source="resctrl"`) on package zones,
the energy formula is:

```text
pod_energy = (AET_core_delta × normFactor) + (uncore × cpuTimeRatio)
```

This differs from the standard ratio model (`activeEnergy × cpuTimeRatio`)
in two important ways:

1. **Core energy** comes from hardware measurement, not CPU-time estimation.
   This captures instruction-mix differences (e.g., AVX-512 vs scalar code)
   that CPU time alone cannot distinguish.
2. **Idle energy** is not separately attributed. The AET core energy counter
   includes all core energy consumed by the pod's processes, whether from
   active computation or shallow idle states (C1). Deep idle energy (C6+)
   is part of the uncore/residual pool distributed by CPU time ratio.

#### System Process Energy in Mixed Mode

In mixed mode (some pods tracked by AET, others not), pool 2 (untracked
core energy = `rootCore − Σ monGroupCore`) contains core energy from:

- Non-AET pods
- System processes (kernel threads, kubelet, containerd, sshd, etc.)

The untracked core pool is distributed among non-AET pods using
`podCPUTime / nonAETCPUTotal` where `nonAETCPUTotal = nodeCPUTime −
aetCPUTotal`. Since system processes contribute to `nonAETCPUTotal` but
are not pods, their share of untracked core energy remains **unattributed**
— it is implicitly absorbed into the denominator without being assigned to
any pod. This is by design: attributing system overhead to user pods would
overstate their consumption.

In **active mode** (Kepler manages resctrl groups), all pods have resctrl
groups, so untracked core energy consists only of system process energy.
This energy stays unattributed.

## Conclusion

Kepler's power attribution system provides practical, proportional distribution
of hardware energy consumption to individual workloads. While CPU-time-based
attribution has inherent limitations due to modern CPU complexity, it offers a
good balance between accuracy, simplicity, and performance overhead for most
monitoring and optimization use cases.

On Intel Xeon processors with AET support (Clearwater Forest and later), the
experimental resctrl/AET attribution path replaces CPU-time estimates with
hardware-measured per-workload core energy, improving accuracy for workloads
with diverse power profiles — particularly those using power-intensive
instruction sets like AVX-512 on hyperthreading-enabled systems.
