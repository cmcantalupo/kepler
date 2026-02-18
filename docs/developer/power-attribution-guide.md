# Kepler Power Attribution Guide

This guide explains how Kepler measures and attributes power consumption to
processes, containers, VMs, and pods running on a system.

## Table of Contents

1. [Bird's Eye View](#birds-eye-view)
2. [Key Concepts](#key-concepts)
3. [Attribution Examples](#attribution-examples)
4. [Implementation Overview](#implementation-overview)
5. [Code Reference](#code-reference)
6. [Resctrl/AET Attribution](#resctrlAET-attribution)
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

This example demonstrates a fundamental limitation of CPU-time ratio
attribution that motivates the hardware-measured AET approach in [Example
6](#example-6-three-pod-resctrlAET-comparison) below.

**System:**

- Intel Xeon 6980P, 128 physical cores, hyperthreading enabled → 256
  logical CPUs
- TDP: 350 W
- Single pod running DGEMM (double-precision matrix multiply) — a
  compute-intensive workload that saturates all physical cores

**What actually happens:**

DGEMM is an ALU-bound workload that cannot benefit from hyperthreading.
Each physical core is 100% busy executing AVX-512 FMA instructions, but
the HT sibling thread on each core has nothing useful to do and sits
idle. The processor draws ~300 W — close to TDP.

Since 128 out of 256 logical CPUs are busy, the OS reports **50% CPU
utilization**, even though the processor is thermally saturated and
consuming nearly its full power budget.

**Kepler's CPU-ratio attribution:**

```text
RAPL total power:    300 W
Node CPU usage ratio: 0.50  (128 busy / 256 logical CPUs)
Active power:        300 × 0.50 = 150 W
Idle power:          300 × 0.50 = 150 W
```

The DGEMM pod is the only workload, so its CPU time ratio ≈ 1.0:

```text
Pod power = 1.0 × 150 W (active) = 150 W
```

**Reality vs attribution:**

| | Reported | Actual | Error |
|---|---------|--------|-------|
| Pod power | 150 W | ~300 W | **+100%** underestimate |
| "Idle" power | 150 W | ~0 W | Entirely misclassified |

Half of the pod's real power consumption is misclassified as idle power
and left unattributed. The 150 W "idle" bucket does not represent genuine
baseline power — it is an artifact of the CPU-ratio model being unable to
distinguish "physical cores saturated with HT siblings idle" from "system
genuinely 50% idle."

**How AET would fix this:**

With resctrl/AET enabled, the root-level `core_energy` counter would
report ~280 W of core energy — directly measured by hardware, unaffected
by the HT illusion. The pod's monitoring group would show the same
~280 W, and the remaining ~20 W (uncore, memory controller, I/O) would
be attributed as residual. Total pod attribution: ~295 W instead of
150 W.

**Takeaway:** Any workload that saturates physical cores without using
hyperthreads — common in HPC, scientific computing, and ML training —
will see its attributed power cut roughly in half by the CPU-ratio model.
This is the single largest source of attribution error in Kepler on
HT-enabled systems, and the primary motivation for hardware-measured AET
attribution described in Example 6.

#### Variant: C-State Idle Cores (HT Disabled)

A related distortion occurs even without hyperthreading when some cores
enter deep sleep states. Consider the same 128-core Xeon with HT
**disabled** (128 logical CPUs = 128 physical cores). A single pod runs
DGEMM on 64 cores; the other 64 cores are completely idle in C6, drawing
near-zero power.

**What actually happens:**

The 64 active cores run AVX-512 at full frequency and consume ~180 W.
The 64 C6 cores are essentially powered off (leakage only, ~2 W total).
RAPL reports ~200 W (180 W cores + 20 W uncore/IO).

**Kepler's CPU-ratio attribution:**

```text
RAPL total power:     200 W
Node CPU usage ratio: 0.50  (64 busy / 128 logical CPUs)
Active power:         200 × 0.50 = 100 W
Idle power:           200 × 0.50 = 100 W
```

The DGEMM pod is the only workload (CPU time ratio ≈ 1.0):

```text
Pod power = 1.0 × 100 W (active) = 100 W
```

**Reality vs attribution:**

| | Reported | Actual | Error |
|---|---------|--------|-------|
| Pod power | 100 W | ~180 W | **~80%** underestimate |
| "Idle" power | 100 W | ~2 W | 50× overestimate |

The model assumes "50% utilization" implies the idle half of the system
is consuming half of total power. In reality, C6 cores draw almost
nothing — the 100 W labeled "idle" is overwhelmingly generated by the
DGEMM cores. The error is smaller than the HT case (80% vs 100%)
because C6 cores genuinely contribute zero `/proc/stat` CPU time,
whereas HT siblings inflate the logical CPU count. But the root cause is
identical: the CPU-ratio model cannot distinguish between "cores drawing
power while idle" and "cores in deep sleep drawing near zero."

**How AET would fix this:**

The root AET `core_energy` counter would report ~180 W, correctly
attributing nearly all core power to the DGEMM pod. The ~20 W residual
(uncore + the negligible C6 leakage) would be shared via CPU-time ratio
— a small and acceptable approximation.

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

| Pod | Workload | CPU Time | CPU Ratio | AET Core Energy |
|-----|----------|----------|-----------|-----------------|
| A (web-app) | HTTP serving | 200 ms | 0.20 | 20 J |
| B (ml-inference) | AVX-heavy model eval | 500 ms | 0.50 | 40 J |
| C (log-shipper) | Lightweight sidecar | 100 ms | 0.10 | 10 J |
| *Other system* | Kernel threads, etc. | 200 ms | 0.20 | — |

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

| Pod | Calculation | Power |
|-----|-------------|-------|
| A | 0.20 × 50 W | **10 W** |
| B | 0.50 × 50 W | **25 W** |
| C | 0.10 × 50 W | **5 W** |
| *Sum* | | *40 W* |

Total attributed to pods: 40 W of 50 W active. The remaining 10 W of
active energy covers non-pod system activity. The 50 W idle energy is
not distributed.

**Observation:** Pod B (ml-inference) and a hypothetical scalar workload
with the same CPU time would receive identical power — the ratio model
cannot distinguish instruction-level power differences.

#### Case 2: Pods A and B Tracked, Pod C Not Tracked

Pods A and B have resctrl monitoring groups with AET data. Pod C does
not. Kepler automatically selects **mixed mode**, which operates against
the **active energy** budget (50 J) to remain consistent with ratio-only
pods.

**Step 1 — Scale raw AET to active-energy units:**

```text
scaled_core_A = 20 J × 0.50 (cpuUsageRatio) = 10 J
scaled_core_B = 40 J × 0.50                  = 20 J
sum_scaled                                    = 30 J
```

**Step 2 — Compute uncore budget and normalization factor:**

```text
uncore   = max(0, activeEnergy − sum_scaled) = max(0, 50 − 30) = 20 J
normFactor = min(1.0, activeEnergy / sum_scaled) = min(1.0, 50/30) = 1.0
```

Since the scaled AET total (30 J) fits within the active budget (50 J),
no normalization is needed.

**Step 3 — Attribute per pod:**

| Pod | Core (normalized) | Uncore Share | Total | Power |
|-----|-------------------|-------------|-------|-------|
| A *(resctrl)* | 10 × 1.0 = 10 J | 20 × 0.20 = 4 J | 14 J | **14 W** |
| B *(resctrl)* | 20 × 1.0 = 20 J | 20 × 0.50 = 10 J | 30 J | **30 W** |
| C *(ratio-only)* | — | 20 × 0.10 = 2 J | 2 J | **2 W** |
| *Sum* | | | | *46 W* |

Pod C receives **only the uncore share** on the package zone — it does
not participate in the core energy budget because it has no AET data.
The remaining 4 J of uncore share (20 × 0.20) covers non-pod system
processes.

**Observations:**
- Pod B's attribution rose from 25 W → 30 W. AET revealed its AVX-heavy
  workload consumes more core energy than CPU time alone would suggest.
- Pod C dropped from 5 W → 2 W. Without AET data in mixed mode, it
  receives only its CPU-time share of the uncore residual.

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

| Pod | Core (raw) | Residual Share | Total | Power |
|-----|-----------|----------------|-------|-------|
| A | 20 × 1.0 = 20 J | 30 × 0.20 = 6 J | 26 J | **26 W** |
| B | 40 × 1.0 = 40 J | 30 × 0.50 = 15 J | 55 J | **55 W** |
| C | 10 × 1.0 = 10 J | 30 × 0.10 = 3 J | 13 J | **13 W** |
| *Sum* | | | | *94 W* |

The remaining 6 J of residual (30 × 0.20) covers non-pod system
processes. Total: 94 + 6 = 100 J = full RAPL budget. ✓

#### Summary Comparison

| Pod | Case 1 (ratio) | Case 2 (mixed) | Case 3 (all-resctrl) |
|-----|---------------|----------------|----------------------|
| A (web-app) | 10 W | 14 W | 26 W |
| B (ml-inference) | 25 W | 30 W | 55 W |
| C (log-shipper) | 5 W | 2 W | 13 W |
| **Pod total** | **40 W** | **46 W** | **94 W** |
| Energy budget | Active (50 W) | Active (50 W) | Full RAPL (100 W) |

**Key takeaways:**

1. **AET exposes instruction-level power differences.** Pod B's AVX-heavy
   ML inference workload consumes far more core energy per CPU cycle than
   the scalar web-app. The ratio model (Case 1) attributes 25 W; AET
   (Case 3) reveals it actually drives 55 W of the system's power.

2. **All-resctrl mode uses a larger budget.** When every pod has AET data,
   the full RAPL delta (including what was previously called "idle") is
   distributed. This is why all Case 3 values are higher — the residual
   (uncore + idle + system overhead) is shared among pods rather than left
   unattributed.

3. **Mixed mode penalizes untracked pods.** In Case 2, Pod C (without AET)
   receives only its CPU-time share of the uncore residual, not the full
   active budget. This ensures that hardware-measured core energy from
   tracked pods is not double-counted.

4. **Energy conservation holds in every case.** The sum of all attributed
   energy (pods + other system processes) never exceeds the RAPL budget.

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

AET counters are exposed at:
```
/sys/fs/resctrl/mon_groups/<group>/mon_data/mon_PERF_PKG_*/core_energy
```

Each `mon_PERF_PKG_*` directory corresponds to a CPU package (socket). The
`core_energy` file contains a cumulative Joule counter of core energy consumed
by the processes assigned to that monitoring group.

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

The residual includes true uncore energy, idle energy, and energy from non-pod
system processes (kernel threads, etc.). Because it is typically a small
fraction of total package energy, the `cpuTimeRatio` split of the residual
has minimal accuracy impact.

**Mixed mode** (`computeBudgetMixed`):

When some pods lack resctrl data:

```text
scaled_core = raw_AET_core × cpuUsageRatio
uncore = max(0, RAPL_activeEnergy − sum(scaled_core))
normFactor = min(1.0, RAPL_activeEnergy / sum(scaled_core))
```

Raw AET deltas are scaled by `cpuUsageRatio` to match `activeEnergy` units,
ensuring both resctrl and ratio pods share a consistent energy budget.

**Normalization**: In both modes, when AET core totals exceed the RAPL budget
(possible due to measurement timing), the uncore budget is zero and
`coreNormFactor` scales pod core values down so their sum equals the RAPL
budget. This preserves relative AET proportions while enforcing conservation.

#### Phase 3 — Attribute (`calculatePodPower` loop)

**File**: `internal/monitor/pod.go`

For each pod × zone combination, one of three branches executes:

| Condition | Branch | Energy Formula |
|-----------|--------|----------------|
| Pod has AET deltas + package zone | Resctrl core | `normCore + uncoreShare × cpuTimeRatio` |
| Pod lacks AET deltas + package zone + resctrl active | Uncore-only | `uncoreShare × cpuTimeRatio` |
| Non-package zone or no resctrl | Pure ratio | `activeEnergy × cpuTimeRatio` |

```go
// Resctrl pod on package zone:
normCore := coreDeltaJoules * ra.coreNormFactor[zone]
uncoreShareJoules := ra.uncoreEnergy[zone] * cpuTimeRatio
activeEnergy = Energy((normCore + uncoreShareJoules) * float64(Joule))

// Non-resctrl pod on package zone (mixed mode):
activeEnergy = Energy(ra.uncoreEnergy[zone] * cpuTimeRatio * float64(Joule))

// Non-package zone or no resctrl data:
activeEnergy = Energy(float64(nodeZoneUsage.activeEnergy) * cpuTimeRatio)
```

Non-resctrl pods receive only the uncore budget share on package zones (not
the full package energy). This ensures energy conservation — measured AET
core plus shared uncore sums to at most the RAPL budget.

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

- **Active mode**: `resctrlSyncGroups()` creates groups for new pods, adds
  PIDs, and deletes groups for terminated pods. Stale groups that fail to
  delete are retained for retry on the next cycle.
- **Passive mode**: `resctrlDiscoverGroups()` lists existing `mon_groups`
  and matches names against running pod UUIDs.

### Key Data Structures

```go
type resctrlAttribution struct {
    coreDelta      map[string]map[int]float64  // [podID][pkgIdx] → raw Joules
    totalCoreByPkg map[int]float64             // [pkgIdx] → sum across pods
    uncoreEnergy   map[EnergyZone]float64      // [zone] → residual Joules
    coreNormFactor map[EnergyZone]float64      // [zone] → conservation factor
    allPodsTracked bool                        // all-resctrl mode flag
    pods           map[string]*Pod             // pre-built pod entries
}
```

The `Pod` struct carries resctrl state:
- `ResctrlCoreEnergyByPkg map[int]float64` — cumulative AET counter per package
- `AttributionSource` — `AttributionRatio` or `AttributionResctrl`

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
- **C2-C6+ (Deep Sleep)**: Progressively deeper sleep states
- **Impact**: Two processes with identical CPU time can have different power
  footprints based on sleep behavior

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

On supported hardware, the [resctrl/AET attribution](#resctrlAET-attribution)
path directly addresses several limitations above by replacing CPU-time-based
core energy estimates with hardware-measured per-workload core energy. This
captures the actual power impact of different instruction mixes, frequency
scaling, and C-state behavior for the core component. The residual (uncore +
idle) still uses the CPU time ratio approximation but is typically a small
fraction of total package energy.

### When to Exercise Caution

- **Mixed workload environments** with varying compute vs I/O patterns
- **High-performance computing** workloads using specialized instructions
- **Absolute power budgeting** decisions based solely on Kepler metrics
- **Fine-grained optimization** requiring precise per-process power
  measurement

### Key Metrics

- `kepler_node_cpu_watts{}`: Total node power consumption
- `kepler_process_cpu_watts{}`: Individual process power
- `kepler_container_cpu_watts{}`: Container-level power
- `kepler_vm_cpu_watts{}`: Virtual machine power
- `kepler_pod_cpu_watts{}`: Kubernetes pod power
- `kepler_pod_resctrl_core_energy_joules_total{}`: Raw AET core energy
  (experimental, only on supported hardware — see
  [Resctrl/AET Attribution](#resctrlAET-attribution))

## Conclusion

Kepler's power attribution system provides practical, proportional distribution
of hardware energy consumption to individual workloads. While CPU-time-based
attribution has inherent limitations due to modern CPU complexity, it offers a
good balance between accuracy, simplicity, and performance overhead for most
monitoring and optimization use cases.

On Intel Xeon processors with AET support, the experimental resctrl/AET
attribution path replaces CPU-time estimates with hardware-measured per-workload
core energy, significantly improving accuracy for workloads with diverse power
profiles.
