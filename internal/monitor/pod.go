// SPDX-FileCopyrightText: 2025 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"github.com/sustainable-computing-io/kepler/internal/resource"
)

// firstPodRead initializes pod power data for the first time
func (pm *PowerMonitor) firstPodRead(snapshot *Snapshot) error {
	running := pm.resources.Pods().Running
	pods := make(Pods, len(running))

	zones := snapshot.Node.Zones
	nodeCPUTimeDelta := pm.resources.Node().ProcessTotalCPUTimeDelta

	// Seed root-level core energy baseline for the three-pool decomposition.
	if pm.resctrlEnabled() {
		rootCoreEnergy, err := pm.resctrl.ReadGroupEnergyByZone("")
		if err == nil && len(rootCoreEnergy) > 0 {
			snapshot.Node.AETCoreBaseline = make(map[int]float64, len(rootCoreEnergy))
			for aetZone, energy := range rootCoreEnergy {
				if pkgIdx, ok := aetZonePackageIndex(aetZone); ok {
					snapshot.Node.AETCoreBaseline[pkgIdx] = energy
				}
			}
		}
	}

	for id, p := range running {
		pod := newPod(p, zones)

		// Seed resctrl baseline energy for pods with monitoring groups.
		// This establishes the per-package cumulative counters so future deltas are correct.
		if pm.hasResctrlGroup(id) {
			pm.resctrlSeedBaseline(pod, id)
		}

		// Calculate initial energy based on CPU ratio * nodeActiveEnergy
		for zone, nodeZoneUsage := range zones {
			if nodeZoneUsage.ActivePower == 0 || nodeZoneUsage.activeEnergy == 0 || nodeCPUTimeDelta == 0 {
				continue
			}

			cpuTimeRatio := p.CPUTimeDelta / nodeCPUTimeDelta
			activeEnergy := Energy(cpuTimeRatio * float64(nodeZoneUsage.activeEnergy))

			pod.Zones[zone] = Usage{
				Power:       Power(0), // No power in first read - no delta time to calculate rate
				EnergyTotal: activeEnergy,
			}
		}

		pods[id] = pod
	}
	// Aggregate GPU power and energy from containers into pods
	for _, container := range snapshot.Containers {
		if container.PodID == "" {
			continue
		}
		if pod, ok := pods[container.PodID]; ok {
			pod.GPUPower += container.GPUPower
			pod.GPUEnergyTotal += container.GPUEnergyTotal
		}
	}

	snapshot.Pods = pods

	pm.logger.Debug("Initialized pod power tracking",
		"pods", len(pods))
	return nil
}

// calculatePodPower calculates pod power for each running pod and handles terminated pods.
//
// When resctrl/AET is enabled, two attribution modes are selected automatically:
//
// All-resctrl mode (every running pod has a resctrl group with valid AET data):
//   - Core energy per pod comes directly from raw AET hardware measurement.
//   - Residual = RAPL_total_delta - sum(raw_AET_core). This residual includes
//     true uncore energy, idle energy, and energy from non-pod system processes.
//   - Each pod's residual share = residual × cpuTimeRatio.
//   - No UsageRatio scaling is applied — the CPU-time linear approximation is
//     confined to the (typically small) residual, preserving AET fidelity.
//
// Mixed mode (some pods lack resctrl groups):
//   - Uses root-level AET core_energy to decompose RAPL into three pools:
//     1. Tracked core: mon_group core deltas → directly to AET pods
//     2. Untracked core: (rootCore - Σ monGroupCore) → shared among non-AET pods
//     3. Uncore: (RAPL - rootCore) → shared among ALL pods by cpuTimeRatio
//   - This guarantees energy conservation: the three pools sum to RAPL delta.
//   - Non-package zones (DRAM, etc.): all pods use pure cpuTimeRatio model.
func (pm *PowerMonitor) calculatePodPower(prev, newSnapshot *Snapshot) error {
	// Clear terminated workloads if snapshot has been exported
	if pm.exported.Load() {
		pm.logger.Debug("Clearing terminated pods after export")
		pm.terminatedPodsTracker.Clear()
	}

	// Get the current pods
	pods := pm.resources.Pods()

	// Handle terminated pods
	pm.logger.Debug("Processing terminated pods", "terminated", len(pods.Terminated))
	for id := range pods.Terminated {
		prevPod, exists := prev.Pods[id]
		if !exists {
			continue
		}

		// Add to internal tracker (which will handle priority-based retention)
		// NOTE: Each terminated pod is only added once since a pod cannot be terminated twice
		pm.terminatedPodsTracker.Add(prevPod.Clone())
	}

	// Skip if no running pods
	if len(pods.Running) == 0 {
		pm.logger.Debug("No running pods found, skipping pod power calculation")
		return nil
	}

	node := pm.resources.Node()
	nodeCPUTimeDelta := node.ProcessTotalCPUTimeDelta

	pm.logger.Debug("Calculating pod power",
		"node-cputime", nodeCPUTimeDelta,
		"running", len(pods.Running),
	)

	// Initialize pod map
	podMap := make(map[string]*Pod, len(pods.Running))

	// ---- Phase 1 & 2: Read AET deltas and compute energy budgets ----
	// Resctrl attribution is handled in resctrl.go. Two modes:
	//   allPodsTracked=true:  raw AET deltas vs total RAPL deltaEnergy (no UsageRatio scaling)
	//   allPodsTracked=false: mixed mode — three-pool decomposition using root core_energy
	var ra *resctrlAttribution
	if pm.resctrlEnabled() {
		ra = pm.resctrlReadDeltas(prev, newSnapshot.Node.Zones, pods)
		ra.resctrlComputeBudget(newSnapshot.Node.Zones)
		// Adopt pod entries created during delta reading (with resctrl metadata).
		for id, pod := range ra.pods {
			podMap[id] = pod
		}
		if len(ra.totalCoreByPkg) > 0 {
			newSnapshot.TotalResctrlCoreEnergyByPkg = ra.totalCoreByPkg
		}
		// Store root core energy cumulative on the node for next cycle's baseline.
		if ra.rootCoreCumulative != nil {
			newSnapshot.Node.AETCoreBaseline = ra.rootCoreCumulative
		}
	}

	// ---- Phase 3: Attribute energy to each pod ----
	for id, p := range pods.Running {
		// Get or create the pod entry (resctrl pods already created above)
		pod, exists := podMap[id]
		if !exists {
			pod = newPod(p, newSnapshot.Node.Zones)
			pod.AttributionSource = AttributionRatio
			podMap[id] = pod
		}

		for zone, nodeZoneUsage := range newSnapshot.Node.Zones {
			// Skip zones where no meaningful attribution is possible.
			// Power == 0 means no sensor data; nodeCPUTimeDelta == 0 means
			// we can't compute cpu time ratios. We do NOT skip on
			// activeEnergy == 0 here because in all-resctrl mode attribution
			// is based on deltaEnergy (total RAPL delta), which can be non-zero
			// even when cpuUsageRatio (and thus activeEnergy) is zero.
			if nodeZoneUsage.Power == 0 || nodeCPUTimeDelta == 0 {
				continue
			}

			cpuTimeRatio := p.CPUTimeDelta / nodeCPUTimeDelta
			var activeEnergy Energy
			var activePower Power

			hasResctrl := ra != nil
			podDeltas, hasDeltas := map[int]float64(nil), false
			if hasResctrl {
				podDeltas, hasDeltas = ra.coreDelta[id]
			}

			if hasDeltas && isPackageZone(zone) {
				// Determine the pod's core energy delta for this RAPL zone.
				// Per-package RAPL zone (e.g., "package-0"): use matching pkgIdx.
				// Aggregated RAPL zone (e.g., "package"): sum across all packages.
				var coreDeltaJoules float64
				if pkgIdx, ok := raplZonePackageIndex(zone); ok {
					coreDeltaJoules = podDeltas[pkgIdx]
				} else {
					for _, v := range podDeltas {
						coreDeltaJoules += v
					}
				}

				// Core from AET (normalized for conservation), uncore share by CPU ratio.
				normCore := coreDeltaJoules * ra.coreNormFactor[zone]
				uncoreShareJoules := ra.uncoreEnergy[zone] * cpuTimeRatio
				activeEnergy = Energy((normCore + uncoreShareJoules) * float64(Joule))
				// Power estimate: proportional to energy ratio vs node.
				// Guard against activeEnergy == 0 which can happen when cpuUsageRatio is 0.
				if nodeZoneUsage.activeEnergy > 0 {
					activePower = Power(float64(activeEnergy) / float64(nodeZoneUsage.activeEnergy) * float64(nodeZoneUsage.ActivePower))
				} else if nodeZoneUsage.deltaEnergy > 0 {
					activePower = Power(float64(activeEnergy) / float64(nodeZoneUsage.deltaEnergy) * float64(nodeZoneUsage.Power))
				}
			} else if hasResctrl && isPackageZone(zone) && len(ra.coreDelta) > 0 {
				// Package zone without resctrl data: non-AET pod.
				// Non-AET pods receive two shares:
				//   1. Untracked core share: (rootCore - Σ monGroupCore) × (podCPU / nonAETCPU)
				//   2. Uncore share: (RAPL - rootCore) × cpuTimeRatio (same as AET pods)
				// The denominator for untracked core includes all non-AET CPU time
				// (non-AET pods + system processes), so system threads get their
				// implicit share without it being attributed to pods.
				var aetCPUTotal float64
				for aetID := range ra.coreDelta {
					if aetPod, ok := pods.Running[aetID]; ok {
						aetCPUTotal += aetPod.CPUTimeDelta
					}
				}
				nonAETCPUTotal := nodeCPUTimeDelta - aetCPUTotal
				var podShareJoules float64
				// Untracked core share
				if nonAETCPUTotal > 0 && ra.untrackedCore[zone] > 0 {
					podShareJoules += ra.untrackedCore[zone] * (p.CPUTimeDelta / nonAETCPUTotal)
				}
				// Uncore share (same pool as AET pods)
				podShareJoules += ra.uncoreEnergy[zone] * cpuTimeRatio
				activeEnergy = Energy(podShareJoules * float64(Joule))
				if nodeZoneUsage.activeEnergy > 0 {
					activePower = Power(float64(activeEnergy) / float64(nodeZoneUsage.activeEnergy) * float64(nodeZoneUsage.ActivePower))
				} else if nodeZoneUsage.deltaEnergy > 0 {
					activePower = Power(float64(activeEnergy) / float64(nodeZoneUsage.deltaEnergy) * float64(nodeZoneUsage.Power))
				}
			} else {
				// Pure ratio model for non-package zones (e.g., DRAM)
				// or when no resctrl data is available at all.
				// Requires activeEnergy > 0; if cpuUsageRatio is 0 both are zero.
				activeEnergy = Energy(float64(nodeZoneUsage.activeEnergy) * cpuTimeRatio)
				activePower = Power(cpuTimeRatio * float64(nodeZoneUsage.ActivePower))
			}

			absoluteEnergy := activeEnergy
			if prevPod, found := prev.Pods[id]; found {
				if prevUsage, hasZone := prevPod.Zones[zone]; hasZone {
					absoluteEnergy += prevUsage.EnergyTotal
				}
			}

			pod.Zones[zone] = Usage{
				EnergyTotal: absoluteEnergy,
				Power:       activePower,
			}
		}
	}

	// Aggregate GPU power and energy from containers into pods
	for _, container := range newSnapshot.Containers {
		if container.PodID == "" {
			continue
		}
		if pod, ok := podMap[container.PodID]; ok {
			pod.GPUPower += container.GPUPower
			pod.GPUEnergyTotal += container.GPUEnergyTotal
		}
	}

	// Update the snapshot
	newSnapshot.Pods = podMap

	// Populate terminated pods from tracker
	newSnapshot.TerminatedPods = pm.terminatedPodsTracker.Items()

	var resctrlPodCount int
	var totalCoreByPkg map[int]float64
	var uncoreByZone map[EnergyZone]float64
	var allTracked bool
	if ra != nil {
		resctrlPodCount = len(ra.coreDelta)
		totalCoreByPkg = ra.totalCoreByPkg
		uncoreByZone = ra.uncoreEnergy
		allTracked = ra.allPodsTracked
	}
	pm.logger.Debug("snapshot updated for pods",
		"running", len(newSnapshot.Pods),
		"terminated", len(newSnapshot.TerminatedPods),
		"resctrl_pods", resctrlPodCount,
		"all_resctrl", allTracked,
		"total_resctrl_core_by_pkg_j", totalCoreByPkg,
		"uncore_by_zone_j", uncoreByZone,
	)

	return nil
}

// newPod creates a new Pod struct with initialized zones from resource.Pod
func newPod(pod *resource.Pod, zones NodeZoneUsageMap) *Pod {
	p := &Pod{
		ID:                pod.ID,
		Name:              pod.Name,
		Namespace:         pod.Namespace,
		CPUTotalTime:      pod.CPUTotalTime,
		AttributionSource: AttributionRatio, // default; overridden to AttributionResctrl when AET succeeds
		Zones:             make(ZoneUsageMap, len(zones)),
	}

	// Initialize each zone with zero values
	for zone := range zones {
		p.Zones[zone] = Usage{
			EnergyTotal: Energy(0),
			Power:       Power(0),
		}
	}

	return p
}
