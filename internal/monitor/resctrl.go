// SPDX-FileCopyrightText: 2026 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/sustainable-computing-io/kepler/internal/resource"
)

// resctrlAttribution holds the results of Phase 1 (reading AET deltas) and
// Phase 2 (computing the per-zone uncore energy budget and normalization factor).
// It is produced by resctrlReadDeltas + resctrlComputeBudget and consumed by
// calculatePodPower's Phase 3 attribution loop.
type resctrlAttribution struct {
	// coreDelta[podID][pkgIndex] = raw core energy delta in Joules for this cycle.
	coreDelta map[string]map[int]float64

	// totalCoreByPkg[pkgIndex] = sum of coreDelta across all pods for a package.
	// In all-resctrl mode these are raw Joules; in mixed mode they are scaled
	// by UsageRatio for consistency with activeEnergy.
	totalCoreByPkg map[int]float64

	// uncoreEnergy[zone] = residual energy (Joules) not accounted for by AET core.
	// In all-resctrl mode: deltaEnergy - totalRawCore (includes idle + true uncore).
	// In mixed mode:       activeEnergy - totalScaledCore (only active portion).
	uncoreEnergy map[EnergyZone]float64

	// coreNormFactor[zone] = min(1.0, budget / totalCore) to ensure conservation.
	coreNormFactor map[EnergyZone]float64

	// allPodsTracked is true when every running pod has a resctrl group, enabling
	// the optimized path that uses raw AET deltas against total RAPL delta instead
	// of the UsageRatio-scaled approximation.
	allPodsTracked bool

	// pods maps pod IDs to partially-initialized Pod structs with resctrl metadata
	// (cumulative counters, attribution source) set during delta reading.
	pods map[string]*Pod
}

// resctrlReadDeltas performs Phase 1: reads AET core energy from resctrl for
// each pod that has a monitoring group and computes raw per-pod per-package
// energy deltas in Joules (float64). Pods with transient read failures are
// created with their previous baseline preserved. Pods without resctrl groups
// are ignored.
//
// The returned resctrlAttribution has coreDelta, totalCoreByPkg, allPodsTracked,
// and pods populated; uncoreEnergy and coreNormFactor are zero-valued and must
// be computed by resctrlComputeBudget.
func (pm *PowerMonitor) resctrlReadDeltas(
	prev *Snapshot,
	nodeZones NodeZoneUsageMap,
	pods *resource.Pods,
) *resctrlAttribution {
	ra := &resctrlAttribution{
		coreDelta:      make(map[string]map[int]float64),
		totalCoreByPkg: make(map[int]float64),
		uncoreEnergy:   make(map[EnergyZone]float64),
		coreNormFactor: make(map[EnergyZone]float64),
		pods:           make(map[string]*Pod, len(pods.Running)),
	}

	resctrlCount := 0
	for id := range pods.Running {
		if !pm.hasResctrlGroup(id) {
			continue
		}
		resctrlCount++

		energyByZone, err := pm.resctrl.ReadGroupEnergyByZone(id)
		if err != nil {
			pm.logger.Warn("Failed to read resctrl energy for pod, falling back to ratio",
				"pod", id, "error", err)
			// Preserve the last known cumulative resctrl core energy baseline so that
			// future deltas remain correct once resctrl reads succeed again.
			pod := newPod(pods.Running[id], nodeZones)
			if prevPod, exists := prev.Pods[id]; exists && prevPod.ResctrlCoreEnergyByPkg != nil {
				// Deep-copy to preserve snapshot immutability.
				copied := make(map[int]float64, len(prevPod.ResctrlCoreEnergyByPkg))
				for pkgIdx, energy := range prevPod.ResctrlCoreEnergyByPkg {
					copied[pkgIdx] = energy
				}
				pod.ResctrlCoreEnergyByPkg = copied
			}
			ra.pods[id] = pod
			continue
		}

		// Check whether a previous baseline exists for this pod, which is
		// required to compute meaningful AET deltas. The first successful
		// read after a pod appears is baseline-only: its cumulative values
		// seed the snapshot for next cycle's delta computation.
		prevPod, hasPrevBaseline := prev.Pods[id]
		if hasPrevBaseline {
			hasPrevBaseline = prevPod.ResctrlCoreEnergyByPkg != nil
		}

		podDeltas := make(map[int]float64, len(energyByZone))
		podCumulative := make(map[int]float64, len(energyByZone))
		for aetZone, energy := range energyByZone {
			pkgIdx, ok := aetZonePackageIndex(aetZone)
			if !ok {
				continue
			}
			podCumulative[pkgIdx] = energy

			if hasPrevBaseline {
				// Calculate per-package delta from previous snapshot.
				// The counter can roll over, producing a negative delta;
				// discard those — treat as zero for this cycle.
				if prevEnergy, hasPkg := prevPod.ResctrlCoreEnergyByPkg[pkgIdx]; hasPkg {
					if energy >= prevEnergy {
						podDeltas[pkgIdx] = energy - prevEnergy
					}
					// else: counter wrapped or reset — treat delta as 0
				}
			}
		}

		// Only contribute deltas when a previous baseline was available;
		// otherwise this is a seed-only cycle for this pod.
		if hasPrevBaseline {
			ra.coreDelta[id] = podDeltas
			for pkgIdx, d := range podDeltas {
				ra.totalCoreByPkg[pkgIdx] += d
			}
		}

		// Stash the per-package cumulative counters; saved to the pod at the end.
		pod := newPod(pods.Running[id], nodeZones)
		pod.ResctrlCoreEnergyByPkg = podCumulative
		// AttributionSource reflects how this cycle's energy was actually
		// attributed. For seed-only cycles (no previous baseline), energy
		// is still computed via the ratio/uncore-share model; mark resctrl
		// only when AET deltas were used.
		if hasPrevBaseline {
			pod.AttributionSource = AttributionResctrl
		}
		ra.pods[id] = pod
	}

	// All-resctrl is true when a previous snapshot exists, every running pod
	// is tracked by resctrl, and every tracked pod contributed deltas (i.e.,
	// had a valid baseline — not just a first-read seed).
	ra.allPodsTracked = prev != nil &&
		resctrlCount > 0 &&
		resctrlCount == len(pods.Running) &&
		len(ra.coreDelta) == resctrlCount

	return ra
}

// resctrlComputeBudget performs Phase 2: computes per-zone uncore energy budget
// and core normalization factor from the raw AET deltas collected in Phase 1.
//
// When allPodsTracked is true (every running pod has measured AET data), the
// budget is derived from the total RAPL deltaEnergy (raw, not scaled by
// UsageRatio). This preserves hardware-measured core energy fidelity — the
// only approximation is the cpuTimeRatio split of the residual (uncore +
// system overhead).
//
// When allPodsTracked is false (mixed resctrl/ratio pods), the raw deltas are
// scaled by UsageRatio to match activeEnergy units, and the budget is derived
// from activeEnergy. This ensures both resctrl and ratio pods share a
// consistent energy budget.
func (ra *resctrlAttribution) resctrlComputeBudget(nodeZones NodeZoneUsageMap, usageRatio float64) {
	if ra.allPodsTracked {
		ra.computeBudgetAllResctrl(nodeZones)
	} else {
		ra.computeBudgetMixed(nodeZones, usageRatio)
	}
}

// computeBudgetAllResctrl computes the budget using total RAPL deltaEnergy
// (not the activeEnergy approximation). Raw AET deltas are used as-is.
//
// residual = deltaEnergy - sum(rawAETCore)
//
// The residual includes true uncore energy, energy from non-pod processes
// (kernel threads, system services), and idle energy. It is distributed to
// pods by cpuTimeRatio, which is acceptable because it is typically a small
// fraction of the total package energy.
//
// The normalization factor ensures conservation: if raw AET core > RAPL total
// (rare, but possible due to counter timing), pod core values are scaled down
// so their sum equals deltaEnergy.
func (ra *resctrlAttribution) computeBudgetAllResctrl(nodeZones NodeZoneUsageMap) {
	for zone, nodeZoneUsage := range nodeZones {
		if !isPackageZone(zone) {
			continue
		}

		var totalCore float64
		if pkgIdx, ok := raplZonePackageIndex(zone); ok {
			totalCore = ra.totalCoreByPkg[pkgIdx]
		} else {
			// Aggregated RAPL zone (e.g., "package"): sum ALL AET packages
			for _, v := range ra.totalCoreByPkg {
				totalCore += v
			}
		}

		raplTotalJoules := nodeZoneUsage.deltaEnergy.Joules()
		if totalCore == 0 {
			if raplTotalJoules > 0 {
				ra.uncoreEnergy[zone] = raplTotalJoules
				ra.coreNormFactor[zone] = 1.0
			}
			continue
		}
		if raplTotalJoules > totalCore {
			ra.uncoreEnergy[zone] = raplTotalJoules - totalCore
			ra.coreNormFactor[zone] = 1.0
		} else {
			// AET core >= RAPL total: normalize so sum(pod core) = deltaEnergy.
			ra.coreNormFactor[zone] = raplTotalJoules / totalCore
		}
	}
}

// computeBudgetMixed scales raw AET deltas by UsageRatio then computes the
// budget from activeEnergy, matching the existing hybrid model.
func (ra *resctrlAttribution) computeBudgetMixed(nodeZones NodeZoneUsageMap, usageRatio float64) {
	// Scale raw deltas by UsageRatio so they are in "active energy" space.
	for podID, podDeltas := range ra.coreDelta {
		for pkgIdx, raw := range podDeltas {
			podDeltas[pkgIdx] = raw * usageRatio
		}
		ra.coreDelta[podID] = podDeltas
	}
	// Recompute totalCoreByPkg with scaled values.
	for pkgIdx := range ra.totalCoreByPkg {
		ra.totalCoreByPkg[pkgIdx] *= usageRatio
	}

	for zone, nodeZoneUsage := range nodeZones {
		if !isPackageZone(zone) {
			continue
		}

		var totalCore float64
		if pkgIdx, ok := raplZonePackageIndex(zone); ok {
			totalCore = ra.totalCoreByPkg[pkgIdx]
		} else {
			for _, v := range ra.totalCoreByPkg {
				totalCore += v
			}
		}

		raplActiveJoules := nodeZoneUsage.activeEnergy.Joules()
		if totalCore == 0 {
			if raplActiveJoules > 0 {
				ra.uncoreEnergy[zone] = raplActiveJoules
				ra.coreNormFactor[zone] = 1.0
			}
			continue
		}
		if raplActiveJoules > totalCore {
			ra.uncoreEnergy[zone] = raplActiveJoules - totalCore
			ra.coreNormFactor[zone] = 1.0
		} else {
			ra.coreNormFactor[zone] = raplActiveJoules / totalCore
		}
	}
}

// resctrlSeedBaseline reads initial AET cumulative counters for pod during
// firstPodRead so that future deltas are computed correctly.
func (pm *PowerMonitor) resctrlSeedBaseline(pod *Pod, podID string) {
	energyByZone, err := pm.resctrl.ReadGroupEnergyByZone(podID)
	if err != nil {
		pm.logger.Warn("Failed to seed resctrl baseline for pod; will use ratio attribution until next cycle",
			"pod", podID, "error", err)
		return
	}
	pod.ResctrlCoreEnergyByPkg = make(map[int]float64, len(energyByZone))
	for aetZone, energy := range energyByZone {
		if pkgIdx, ok := aetZonePackageIndex(aetZone); ok {
			pod.ResctrlCoreEnergyByPkg[pkgIdx] = energy
		}
	}
	// Do not set AttributionResctrl here — this is a seed-only read;
	// AET deltas are not available until the next cycle.
}

// resctrlEnabled returns true if resctrl/AET monitoring is configured.
func (pm *PowerMonitor) resctrlEnabled() bool {
	return pm.resctrl != nil
}

// hasResctrlGroup returns true if the given pod ID has an active resctrl monitoring group.
func (pm *PowerMonitor) hasResctrlGroup(podID string) bool {
	return pm.resctrlGroups[podID]
}

// manageResctrlGroups creates/discovers resctrl monitoring groups for pods.
// In passive mode, it discovers existing UUID-named groups.
// In active mode, it creates groups for new pods and cleans up terminated pods.
func (pm *PowerMonitor) manageResctrlGroups() {
	if !pm.resctrlEnabled() {
		return
	}

	pods := pm.resources.Pods()

	if pm.resctrlPassiveMode {
		pm.discoverResctrlGroups(pods)
	} else {
		pm.syncResctrlGroups(pods)
	}
}

// discoverResctrlGroups scans for existing UUID-named mon_groups and matches
// them to running pods (passive mode).
func (pm *PowerMonitor) discoverResctrlGroups(pods *resource.Pods) {
	discovered, err := pm.resctrl.DiscoverGroups()
	if err != nil {
		pm.logger.Warn("Failed to discover resctrl groups", "error", err)
		return
	}

	// Build new groups set from discovered groups that match running pods
	newGroups := make(map[string]bool)
	for groupID := range discovered {
		if _, isRunning := pods.Running[groupID]; isRunning {
			newGroups[groupID] = true
		}
	}

	pm.resctrlGroups = newGroups

	unmatchedCount := len(discovered) - len(newGroups)
	if unmatchedCount > 0 && unmatchedCount != pm.prevUnmatchedResctrl {
		// Log only when the count changes to avoid per-cycle spam in passive mode
		// where external orchestrators routinely create groups before Kepler sees pods.
		pm.logger.Info("Discovered resctrl groups not matching any running pod",
			"unmatched", unmatchedCount,
			"total_discovered", len(discovered),
			"matched_pods", len(newGroups))
	} else {
		pm.logger.Debug("Discovered resctrl groups",
			"total_discovered", len(discovered),
			"matched_pods", len(newGroups))
	}
	pm.prevUnmatchedResctrl = unmatchedCount
}

// reconcileResctrlGroups scans the filesystem for pre-existing mon_groups
// and either adopts them (if they belong to still-running pods) or removes
// them (orphans from a previous daemon instance). This runs once on the
// first active-mode sync after startup to recover from non-graceful shutdown.
func (pm *PowerMonitor) reconcileResctrlGroups(pods *resource.Pods) {
	discovered, err := pm.resctrl.DiscoverGroups()
	if err != nil {
		pm.logger.Warn("Failed to discover existing resctrl groups for reconciliation", "error", err)
		return
	}

	adopted, removed := 0, 0
	for groupID := range discovered {
		if _, isRunning := pods.Running[groupID]; isRunning {
			pm.resctrlGroups[groupID] = true
			adopted++
		} else {
			if err := pm.resctrl.DeleteMonitorGroup(groupID); err != nil {
				pm.logger.Warn("Failed to remove orphaned resctrl group",
					"group", groupID, "error", err)
			} else {
				removed++
			}
		}
	}

	if adopted > 0 || removed > 0 {
		pm.logger.Info("Reconciled pre-existing resctrl groups",
			"adopted", adopted, "removed", removed,
			"total_discovered", len(discovered))
	}
}

// cleanupResctrlGroups removes all mon_groups tracked by this daemon
// instance. Called during graceful shutdown in active mode to free kernel
// RMIDs so they are not leaked across restarts.
func (pm *PowerMonitor) cleanupResctrlGroups() {
	cleaned, failed := 0, 0
	for podID := range pm.resctrlGroups {
		if err := pm.resctrl.DeleteMonitorGroup(podID); err != nil {
			pm.logger.Warn("Failed to delete resctrl group during shutdown",
				"pod", podID, "error", err)
			failed++
		} else {
			cleaned++
		}
	}
	pm.resctrlGroups = make(map[string]bool)
	pm.logger.Info("Cleaned up resctrl groups on shutdown",
		"cleaned", cleaned, "failed", failed)
}

// syncResctrlGroups creates groups for new pods and deletes groups for
// terminated pods (active mode). On the first invocation after startup,
// it reconciles pre-existing groups left by a previous daemon instance.
func (pm *PowerMonitor) syncResctrlGroups(pods *resource.Pods) {
	// One-time reconciliation on first sync after startup.
	if !pm.resctrlReconciled {
		pm.reconcileResctrlGroups(pods)
		pm.resctrlReconciled = true
	}
	// Delete groups for terminated pods
	for podID := range pods.Terminated {
		if pm.resctrlGroups[podID] {
			if err := pm.resctrl.DeleteMonitorGroup(podID); err != nil {
				pm.logger.Warn("Failed to delete resctrl group for terminated pod; will retry next cycle",
					"pod", podID, "error", err)
				continue
			}
			delete(pm.resctrlGroups, podID)
		}
	}

	// Create or update groups for running pods.
	// New containers may start in an existing pod after the initial sync,
	// so we re-sync PIDs for already-tracked pods too. Child processes of
	// tracked PIDs are inherited automatically by the kernel (fork copies
	// closid/rmid), but new container init processes need explicit addition.
	//
	// PID assignment race: There is a small window between scanning PIDs
	// and writing them to the mon_group's tasks file. If a process forks
	// during that window, the child inherits the default RMID — not the
	// group's — because the parent was not yet assigned when the fork
	// occurred. One approach is an immediate re-scan after the write to
	// catch stragglers. Kepler does not need an intra-cycle re-scan
	// because this function runs every collection cycle: the per-cycle
	// AddPIDsToGroup call below catches any PIDs missed on the previous
	// cycle. The worst-case impact is one collection interval of
	// misattributed energy for those PIDs.
	//
	// We use containers (not processes) for pod→PID mapping because
	// process.Container.Pod is not populated by the resource informer —
	// only the cached container instances in containers.Running have .Pod set.
	processes := pm.resources.Processes()
	containers := pm.resources.Containers()
	podPIDs := collectAllPodPIDs(processes, containers)

	created, updated, skippedNoPIDs := 0, 0, 0
	for podID := range pods.Running {
		pids := podPIDs[podID]
		if len(pids) == 0 {
			skippedNoPIDs++
			continue
		}

		if pm.resctrlGroups[podID] {
			// Group exists — re-sync PIDs so new containers are captured.
			// Writing already-present PIDs is a kernel no-op. This re-sync
			// also catches any PIDs that forked between the previous cycle's
			// scan and write (see "PID assignment race" comment above).
			if err := pm.resctrl.AddPIDsToGroup(podID, pids); err != nil {
				pm.logger.Warn("Failed to update resctrl group PIDs",
					"pod", podID, "error", err)
			}
			updated++
			continue
		}

		if err := pm.resctrl.CreateMonitorGroup(podID, pids); err != nil {
			pm.logger.Warn("Failed to create resctrl group for pod",
				"pod", podID, "pids", len(pids), "error", err)
			continue
		}
		pm.resctrlGroups[podID] = true
		created++
	}

	// Safety net: clean up any tracked groups whose pod is no longer running
	// and was not seen in Terminated (e.g., if resource tracking dropped it).
	// In normal operation the terminated-pod block above handles cleanup;
	// this catches edge cases where a pod vanishes without a Terminated event.
	for podID := range pm.resctrlGroups {
		if _, isRunning := pods.Running[podID]; !isRunning {
			if err := pm.resctrl.DeleteMonitorGroup(podID); err != nil {
				pm.logger.Debug("Failed to clean up stale resctrl group; will retry next cycle",
					"pod", podID, "error", err)
				// Keep the group tracked so we can retry cleanup in future cycles.
				continue
			}
			delete(pm.resctrlGroups, podID)
		}
	}

	if created > 0 {
		pm.logger.Info("Synced resctrl groups",
			"resctrl_pods", len(pm.resctrlGroups),
			"created", created,
			"skipped_no_pids", skippedNoPIDs,
			"pods_running", len(pods.Running),
			"pods_with_pids", len(podPIDs))
	}
	pm.logger.Debug("Synced resctrl groups",
		"resctrl_pods", len(pm.resctrlGroups),
		"created", created,
		"updated", updated,
		"pods_running", len(pods.Running))
}

// collectAllPodPIDs builds a map from pod ID → PIDs by joining processes
// with containers. We cannot use proc.Container.Pod directly because the
// resource informer only sets .Pod on the cached container instances
// (containers.Running), not on the container references held by processes.
// Instead, we build a containerID→podID index from containers, then match
// each process's container ID to find its pod.
func collectAllPodPIDs(processes *resource.Processes, containers *resource.Containers) map[string][]int {
	// Build containerID → podID index from containers (which have .Pod set)
	containerToPod := make(map[string]string, len(containers.Running))
	for _, c := range containers.Running {
		if c.Pod != nil {
			containerToPod[c.ID] = c.Pod.ID
		}
	}

	// Walk processes and match by container ID
	podPIDs := make(map[string][]int)
	for pid, proc := range processes.Running {
		if proc.Container == nil {
			continue
		}
		podID, ok := containerToPod[proc.Container.ID]
		if !ok {
			continue
		}
		podPIDs[podID] = append(podPIDs[podID], pid)
	}
	return podPIDs
}

// isPackageZone returns true if the zone represents a CPU package zone.
// Package zones contain both core and uncore energy. Zone names like
// "package-0", "package-1", or "psys" are package zones.
func isPackageZone(zone EnergyZone) bool {
	lower := strings.ToLower(zone.Name())
	return strings.Contains(lower, "package") || strings.Contains(lower, "psys")
}

// aetZonePkgRegexp matches the trailing numeric index from AET zone names
// like "mon_PERF_PKG_00" or "mon_PERF_PKG_1".
var aetZonePkgRegexp = regexp.MustCompile(`mon_PERF_PKG_(\d+)$`)

// aetZonePackageIndex extracts the package index from an AET zone name.
// For example, "mon_PERF_PKG_00" → 0, "mon_PERF_PKG_1" → 1.
func aetZonePackageIndex(zoneName string) (int, bool) {
	m := aetZonePkgRegexp.FindStringSubmatch(zoneName)
	if m == nil {
		return 0, false
	}
	idx, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return idx, true
}

// raplZonePkgRegexp matches the numeric index from RAPL zone names
// like "package-0" or "Package-1". Case-insensitive and anchored to
// avoid partial matches.
var raplZonePkgRegexp = regexp.MustCompile(`(?i)^package-(\d+)$`)

// raplZonePackageIndex extracts the package index from a RAPL EnergyZone.
// For example, "package-0" → 0, "package-1" → 1.
// Returns 0, false for non-package zones (e.g., "dram-0", "psys").
func raplZonePackageIndex(zone EnergyZone) (int, bool) {
	m := raplZonePkgRegexp.FindStringSubmatch(zone.Name())
	if m == nil {
		return 0, false
	}
	idx, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return idx, true
}
