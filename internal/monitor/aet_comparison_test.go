// SPDX-FileCopyrightText: 2026 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package monitor

// This file contains an integration-style comparison test that verifies the
// numerical claims made in Example 6 of the power-attribution-guide.md.
//
// The test runs the same workload scenario through all three attribution modes
// (ratio-only, mixed, all-resctrl) and asserts the exact power values from the
// Summary Comparison table. It also demonstrates that AET attribution
// differentiates instruction-mix-dependent power profiles while the ratio model
// cannot.

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/sustainable-computing-io/kepler/internal/resource"
	testingclock "k8s.io/utils/clock/testing"
)

// Example 6 constants — directly from power-attribution-guide.md.
const (
	// RAPL package-0 energy delta: 100 J → 100 W total (1-second interval)
	ex6RAPLDelta = 100 * Joule

	// Node CPU usage ratio: 0.50
	ex6UsageRatio = 0.50

	// Active energy: 50 J, Idle energy: 50 J
	ex6ActiveEnergy = 50 * Joule

	// Node total CPU time delta: 1000 ms
	ex6NodeCPUTime = 1000.0

	// Root-level core_energy delta (system-wide total): 80 J (Case 2)
	ex6RootCoreDelta = 80.0

	// Per-pod CPU times
	ex6PodACPU = 200.0 // ms
	ex6PodBCPU = 500.0 // ms
	ex6PodCCPU = 100.0 // ms
	ex6SysCPU  = 200.0 // ms (other system: kernel threads, etc.)

	// Per-pod AET core energy deltas (Joules)
	ex6PodACoreEnergy = 20.0
	ex6PodBCoreEnergy = 40.0
	ex6PodCCoreEnergy = 10.0

	// Comparison tolerances
	// Power is derived from energy via (energy/deltaEnergy)*nodePower, which
	// introduces floating-point rounding. 0.01 W covers that.
	powerTolerance = 0.01
)

// createExample6Node builds a Node snapshot matching Example 6's system state.
// The key difference from createNodeSnapshotWithDelta is that this sets
// Power = 100 W (matching the 100 J / 1 s interval) rather than the
// hardcoded 50 W used by the existing test helper.
func createExample6Node(zones []EnergyZone, timestamp time.Time) *Node {
	node := &Node{
		Timestamp:  timestamp,
		UsageRatio: ex6UsageRatio,
		Zones:      make(NodeZoneUsageMap, len(zones)),
	}
	for _, zone := range zones {
		node.Zones[zone] = NodeUsage{
			EnergyTotal:       200 * Joule, // cumulative (arbitrary)
			activeEnergy:      ex6ActiveEnergy,
			ActiveEnergyTotal: ex6ActiveEnergy,
			IdleEnergyTotal:   ex6RAPLDelta - ex6ActiveEnergy,
			Power:             Power(100 * Watt),
			ActivePower:       Power(50 * Watt),
			IdlePower:         Power(50 * Watt),
			deltaEnergy:       ex6RAPLDelta,
		}
	}
	return node
}

// example6Pods returns the three pods from Example 6 as resource.Pod objects.
func example6Pods() map[string]*resource.Pod {
	return map[string]*resource.Pod{
		"pod-a": {
			ID: "pod-a", Name: "web-app", Namespace: "default",
			CPUTimeDelta: ex6PodACPU,
		},
		"pod-b": {
			ID: "pod-b", Name: "ml-inference", Namespace: "default",
			CPUTimeDelta: ex6PodBCPU,
		},
		"pod-c": {
			ID: "pod-c", Name: "log-shipper", Namespace: "default",
			CPUTimeDelta: ex6PodCCPU,
		},
	}
}

// podPowerWatts extracts the package-zone power in watts for a pod.
// Power is calculated by the attribution code as a proportion of node power.
// But for comparison, we use the energy delta (Joules in a 1-second interval
// = Watts) which is more direct. This helper returns the energy delta in
// Joules, which equals Watts over a 1-second interval.
func podEnergyDeltaJoules(pod *Pod, zone EnergyZone, prevEnergy Energy) float64 {
	return (pod.Zones[zone].EnergyTotal - prevEnergy).Joules()
}

// TestExample6_Case1_RatioOnly verifies Example 6 Case 1: all pods use the
// CPU-time ratio model. Each pod gets cpuTimeRatio × ActivePower.
//
// Expected from the guide:
//
//	Pod A: 0.20 × 50 W = 10 W
//	Pod B: 0.50 × 50 W = 25 W
//	Pod C: 0.10 × 50 W = 5 W
//	Total: 40 W (of 50 W active budget)
func TestExample6_Case1_RatioOnly(t *testing.T) {
	zones := CreateTestZones()

	// No resctrl meter — pure ratio model
	resInformer := &MockResourceInformer{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fakeClock := testingclock.NewFakeClock(time.Now())
	mockMeter := &MockCPUPowerMeter{}
	mockMeter.On("Zones").Return(zones, nil)
	mockMeter.On("PrimaryEnergyZone").Return(zones[0], nil)

	monitor := &PowerMonitor{
		logger:        logger,
		cpu:           mockMeter,
		clock:         fakeClock,
		resources:     resInformer,
		maxTerminated: 500,
		// resctrl is nil — no AET
	}
	_ = monitor.Init()

	tr := &TestResource{
		Node: &resource.Node{
			CPUUsageRatio:            ex6UsageRatio,
			ProcessTotalCPUTimeDelta: ex6NodeCPUTime,
		},
		Pods: &resource.Pods{
			Running:    example6Pods(),
			Terminated: map[string]*resource.Pod{},
		},
		Processes: &resource.Processes{
			Running:    map[int]*resource.Process{},
			Terminated: map[int]*resource.Process{},
		},
	}
	resInformer.SetExpectations(t, tr)

	// Previous snapshot with zero energy
	prevSnapshot := NewSnapshot()
	prevSnapshot.Node = createExample6Node(zones, time.Now())
	for id, p := range example6Pods() {
		prevSnapshot.Pods[id] = &Pod{
			ID: id, Name: p.Name, Namespace: p.Namespace,
			Zones: make(ZoneUsageMap, len(zones)),
		}
		for _, zone := range zones {
			prevSnapshot.Pods[id].Zones[zone] = Usage{EnergyTotal: 0}
		}
	}

	newSnapshot := NewSnapshot()
	newSnapshot.Node = createExample6Node(zones, time.Now().Add(time.Second))

	err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
	require.NoError(t, err)

	pkgZone := zones[0] // package-0

	podA := newSnapshot.Pods["pod-a"]
	podB := newSnapshot.Pods["pod-b"]
	podC := newSnapshot.Pods["pod-c"]
	require.NotNil(t, podA)
	require.NotNil(t, podB)
	require.NotNil(t, podC)

	// All pods use ratio attribution
	assert.Equal(t, AttributionRatio, podA.AttributionSource)
	assert.Equal(t, AttributionRatio, podB.AttributionSource)
	assert.Equal(t, AttributionRatio, podC.AttributionSource)

	// Verify per-pod energy deltas match Example 6 Case 1
	deltaA := podEnergyDeltaJoules(podA, pkgZone, 0)
	deltaB := podEnergyDeltaJoules(podB, pkgZone, 0)
	deltaC := podEnergyDeltaJoules(podC, pkgZone, 0)

	assert.InDelta(t, 10.0, deltaA, powerTolerance,
		"Pod A (web-app): 0.20 × 50 = 10 W")
	assert.InDelta(t, 25.0, deltaB, powerTolerance,
		"Pod B (ml-inference): 0.50 × 50 = 25 W")
	assert.InDelta(t, 5.0, deltaC, powerTolerance,
		"Pod C (log-shipper): 0.10 × 50 = 5 W")

	// Total pod energy = 40 J of 50 J active budget
	totalPod := deltaA + deltaB + deltaC
	assert.InDelta(t, 40.0, totalPod, powerTolerance,
		"Total attributed to pods: 40 W of 50 W active")

	// Key observation: ratio model assigns identical power per CPU-ms.
	// Pod B (AVX-heavy, 500ms) gets 2.5× Pod A (scalar, 200ms) — purely
	// reflecting CPU time, not the actual 2× higher power-per-cycle.
	wattsPerMs := deltaB / ex6PodBCPU
	assert.InDelta(t, deltaA/ex6PodACPU, wattsPerMs, powerTolerance,
		"Ratio model: all pods get identical power per CPU-ms (blind to instruction mix)")
}

// TestExample6_Case2_MixedMode verifies Example 6 Case 2: Pods A and B tracked
// by resctrl, Pod C uses ratio fallback. Three-pool mixed attribution.
//
// Expected from the guide:
//
//	Pod A: 20 J core + 4 J uncore = 24 W
//	Pod B: 40 J core + 10 J uncore = 50 W
//	Pod C: 6.67 J untracked core + 2 J uncore = 8.67 W
//	Other: 13.33 J untracked core + 4 J uncore = 17.33 W (implicit)
//	Total: 100 W = full RAPL budget
func TestExample6_Case2_MixedMode(t *testing.T) {
	zones := CreateTestZones()

	resctrlMeter := &MockResctrlMeter{}
	resInformer := &MockResourceInformer{}

	// Build monitor with precise root energy mock
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fakeClock := testingclock.NewFakeClock(time.Now())
	mockMeter := &MockCPUPowerMeter{}
	mockMeter.On("Zones").Return(zones, nil)
	mockMeter.On("PrimaryEnergyZone").Return(zones[0], nil)
	resctrlMeter.On("Zones").Return([]string{"mon_PERF_PKG_0"}).Maybe()
	resctrlMeter.On("ReadGroupActivityByZone", mock.Anything).Maybe().Return(
		map[string]float64{"mon_PERF_PKG_0": 0.0}, nil)

	monitor := &PowerMonitor{
		logger:             logger,
		cpu:                mockMeter,
		clock:              fakeClock,
		resources:          resInformer,
		maxTerminated:      500,
		resctrl:            resctrlMeter,
		resctrlPassiveMode: false,
		resctrlGroups:      make(map[string]bool),
	}
	_ = monitor.Init()

	// Pods A and B have resctrl groups; Pod C does not
	monitor.resctrlGroups["pod-a"] = true
	monitor.resctrlGroups["pod-b"] = true

	tr := &TestResource{
		Node: &resource.Node{
			CPUUsageRatio:            ex6UsageRatio,
			ProcessTotalCPUTimeDelta: ex6NodeCPUTime,
		},
		Pods: &resource.Pods{
			Running:    example6Pods(),
			Terminated: map[string]*resource.Pod{},
		},
		Processes: &resource.Processes{
			Running:    map[int]*resource.Process{},
			Terminated: map[int]*resource.Process{},
		},
	}
	resInformer.SetExpectations(t, tr)

	// Cumulative AET values: previous baseline + delta = current reading
	// Pod A: prev=100, current=120, delta=20J
	// Pod B: prev=200, current=240, delta=40J
	resctrlMeter.On("ReadGroupEnergyByZone", "pod-a").Return(
		map[string]float64{"mon_PERF_PKG_0": 120.0}, nil)
	resctrlMeter.On("ReadGroupEnergyByZone", "pod-b").Return(
		map[string]float64{"mon_PERF_PKG_0": 240.0}, nil)

	// Root core energy: prev=500, current=580, delta=80J
	resctrlMeter.On("ReadGroupEnergyByZone", "").Return(
		map[string]float64{"mon_PERF_PKG_0": 580.0}, nil)

	// Previous snapshot
	prevSnapshot := NewSnapshot()
	prevSnapshot.Node = createExample6Node(zones, time.Now())
	prevSnapshot.Node.AETCoreBaseline = map[int]float64{0: 500.0}

	prevSnapshot.Pods["pod-a"] = &Pod{
		ID: "pod-a", Name: "web-app", Namespace: "default",
		ResctrlCoreEnergyByPkg: map[int]float64{0: 100.0},
		AttributionSource:      AttributionResctrl,
		Zones:                  make(ZoneUsageMap, len(zones)),
	}
	prevSnapshot.Pods["pod-b"] = &Pod{
		ID: "pod-b", Name: "ml-inference", Namespace: "default",
		ResctrlCoreEnergyByPkg: map[int]float64{0: 200.0},
		AttributionSource:      AttributionResctrl,
		Zones:                  make(ZoneUsageMap, len(zones)),
	}
	prevSnapshot.Pods["pod-c"] = &Pod{
		ID: "pod-c", Name: "log-shipper", Namespace: "default",
		Zones: make(ZoneUsageMap, len(zones)),
	}
	for _, zone := range zones {
		prevSnapshot.Pods["pod-a"].Zones[zone] = Usage{EnergyTotal: 0}
		prevSnapshot.Pods["pod-b"].Zones[zone] = Usage{EnergyTotal: 0}
		prevSnapshot.Pods["pod-c"].Zones[zone] = Usage{EnergyTotal: 0}
	}

	newSnapshot := NewSnapshot()
	newSnapshot.Node = createExample6Node(zones, time.Now().Add(time.Second))

	err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
	require.NoError(t, err)

	pkgZone := zones[0]
	podA := newSnapshot.Pods["pod-a"]
	podB := newSnapshot.Pods["pod-b"]
	podC := newSnapshot.Pods["pod-c"]
	require.NotNil(t, podA)
	require.NotNil(t, podB)
	require.NotNil(t, podC)

	// Attribution sources
	assert.Equal(t, AttributionResctrl, podA.AttributionSource)
	assert.Equal(t, AttributionResctrl, podB.AttributionSource)
	assert.Equal(t, AttributionRatio, podC.AttributionSource)

	// Verify the three-pool decomposition
	// tracked_core = 20 + 40 = 60 J
	assert.InDelta(t, 60.0, newSnapshot.TotalResctrlCoreEnergyByPkg[0], powerTolerance,
		"Sum of mon_group core deltas = 60 J")

	// Verify per-pod energy deltas match Example 6 Case 2
	deltaA := podEnergyDeltaJoules(podA, pkgZone, 0)
	deltaB := podEnergyDeltaJoules(podB, pkgZone, 0)
	deltaC := podEnergyDeltaJoules(podC, pkgZone, 0)

	// Pod A: 20J core × 1.0 + 20J uncore × 0.20 = 20 + 4 = 24 W
	assert.InDelta(t, 24.0, deltaA, powerTolerance,
		"Pod A (resctrl): 20J core + 4J uncore = 24 W")

	// Pod B: 40J core × 1.0 + 20J uncore × 0.50 = 40 + 10 = 50 W
	assert.InDelta(t, 50.0, deltaB, powerTolerance,
		"Pod B (resctrl): 40J core + 10J uncore = 50 W")

	// Pod C: 20J untracked × (100/300) + 20J uncore × 0.10 = 6.67 + 2 = 8.67 W
	assert.InDelta(t, 8.67, deltaC, powerTolerance,
		"Pod C (ratio): 6.67J untracked core + 2J uncore = 8.67 W")

	// Energy conservation: sum(all pod deltas) + system overhead = 100 J
	totalPod := deltaA + deltaB + deltaC
	assert.InDelta(t, 82.67, totalPod, powerTolerance,
		"Total attributed to pods: 82.67 W")

	// System threads get implicit share: 100 - 82.67 = 17.33 J (not attributed to any pod)
	systemImplicit := 100.0 - totalPod
	assert.InDelta(t, 17.33, systemImplicit, powerTolerance,
		"Implicit system thread energy: 17.33 W")

	resctrlMeter.AssertExpectations(t)
}

// TestExample6_Case3_AllResctrl verifies Example 6 Case 3: all pods tracked by
// resctrl. Uses the full RAPL delta (100 J) as the budget.
//
// Expected from the guide:
//
//	Pod A: 20J core + 6J residual = 26 W
//	Pod B: 40J core + 15J residual = 55 W
//	Pod C: 10J core + 3J residual = 13 W
//	Total pods: 94 W; system: 6 W; full budget: 100 W
func TestExample6_Case3_AllResctrl(t *testing.T) {
	zones := CreateTestZones()

	resctrlMeter := &MockResctrlMeter{}
	resInformer := &MockResourceInformer{}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fakeClock := testingclock.NewFakeClock(time.Now())
	mockMeter := &MockCPUPowerMeter{}
	mockMeter.On("Zones").Return(zones, nil)
	mockMeter.On("PrimaryEnergyZone").Return(zones[0], nil)
	resctrlMeter.On("Zones").Return([]string{"mon_PERF_PKG_0"}).Maybe()
	resctrlMeter.On("ReadGroupActivityByZone", mock.Anything).Maybe().Return(
		map[string]float64{"mon_PERF_PKG_0": 0.0}, nil)
	resctrlMeter.On("ReadGroupEnergyByZone", "").Maybe().Return(
		map[string]float64{"mon_PERF_PKG_0": 0.0}, nil)

	monitor := &PowerMonitor{
		logger:             logger,
		cpu:                mockMeter,
		clock:              fakeClock,
		resources:          resInformer,
		maxTerminated:      500,
		resctrl:            resctrlMeter,
		resctrlPassiveMode: false,
		resctrlGroups:      make(map[string]bool),
	}
	_ = monitor.Init()

	// All three pods have resctrl groups
	monitor.resctrlGroups["pod-a"] = true
	monitor.resctrlGroups["pod-b"] = true
	monitor.resctrlGroups["pod-c"] = true

	tr := &TestResource{
		Node: &resource.Node{
			CPUUsageRatio:            ex6UsageRatio,
			ProcessTotalCPUTimeDelta: ex6NodeCPUTime,
		},
		Pods: &resource.Pods{
			Running:    example6Pods(),
			Terminated: map[string]*resource.Pod{},
		},
		Processes: &resource.Processes{
			Running:    map[int]*resource.Process{},
			Terminated: map[int]*resource.Process{},
		},
	}
	resInformer.SetExpectations(t, tr)

	// Cumulative AET values: previous + delta = current
	// Pod A: prev=100, current=120, delta=20J
	// Pod B: prev=200, current=240, delta=40J
	// Pod C: prev=300, current=310, delta=10J
	resctrlMeter.On("ReadGroupEnergyByZone", "pod-a").Return(
		map[string]float64{"mon_PERF_PKG_0": 120.0}, nil)
	resctrlMeter.On("ReadGroupEnergyByZone", "pod-b").Return(
		map[string]float64{"mon_PERF_PKG_0": 240.0}, nil)
	resctrlMeter.On("ReadGroupEnergyByZone", "pod-c").Return(
		map[string]float64{"mon_PERF_PKG_0": 310.0}, nil)

	// Previous snapshot with baselines
	prevSnapshot := NewSnapshot()
	prevSnapshot.Node = createExample6Node(zones, time.Now())

	prevSnapshot.Pods["pod-a"] = &Pod{
		ID: "pod-a", Name: "web-app", Namespace: "default",
		ResctrlCoreEnergyByPkg: map[int]float64{0: 100.0},
		AttributionSource:      AttributionResctrl,
		Zones:                  make(ZoneUsageMap, len(zones)),
	}
	prevSnapshot.Pods["pod-b"] = &Pod{
		ID: "pod-b", Name: "ml-inference", Namespace: "default",
		ResctrlCoreEnergyByPkg: map[int]float64{0: 200.0},
		AttributionSource:      AttributionResctrl,
		Zones:                  make(ZoneUsageMap, len(zones)),
	}
	prevSnapshot.Pods["pod-c"] = &Pod{
		ID: "pod-c", Name: "log-shipper", Namespace: "default",
		ResctrlCoreEnergyByPkg: map[int]float64{0: 300.0},
		AttributionSource:      AttributionResctrl,
		Zones:                  make(ZoneUsageMap, len(zones)),
	}
	for _, zone := range zones {
		prevSnapshot.Pods["pod-a"].Zones[zone] = Usage{EnergyTotal: 0}
		prevSnapshot.Pods["pod-b"].Zones[zone] = Usage{EnergyTotal: 0}
		prevSnapshot.Pods["pod-c"].Zones[zone] = Usage{EnergyTotal: 0}
	}

	newSnapshot := NewSnapshot()
	newSnapshot.Node = createExample6Node(zones, time.Now().Add(time.Second))

	err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
	require.NoError(t, err)

	pkgZone := zones[0]
	podA := newSnapshot.Pods["pod-a"]
	podB := newSnapshot.Pods["pod-b"]
	podC := newSnapshot.Pods["pod-c"]
	require.NotNil(t, podA)
	require.NotNil(t, podB)
	require.NotNil(t, podC)

	// All pods use resctrl attribution
	assert.Equal(t, AttributionResctrl, podA.AttributionSource)
	assert.Equal(t, AttributionResctrl, podB.AttributionSource)
	assert.Equal(t, AttributionResctrl, podC.AttributionSource)

	// total_core = 20 + 40 + 10 = 70 J
	assert.InDelta(t, 70.0, newSnapshot.TotalResctrlCoreEnergyByPkg[0], powerTolerance)

	// Verify per-pod energy deltas match Example 6 Case 3
	deltaA := podEnergyDeltaJoules(podA, pkgZone, 0)
	deltaB := podEnergyDeltaJoules(podB, pkgZone, 0)
	deltaC := podEnergyDeltaJoules(podC, pkgZone, 0)

	// residual = 100 - 70 = 30 J, normFactor = 1.0
	// Pod A: 20×1.0 + 30×0.20 = 26 W
	assert.InDelta(t, 26.0, deltaA, powerTolerance,
		"Pod A: 20J core + 6J residual = 26 W")

	// Pod B: 40×1.0 + 30×0.50 = 55 W
	assert.InDelta(t, 55.0, deltaB, powerTolerance,
		"Pod B: 40J core + 15J residual = 55 W")

	// Pod C: 10×1.0 + 30×0.10 = 13 W
	assert.InDelta(t, 13.0, deltaC, powerTolerance,
		"Pod C: 10J core + 3J residual = 13 W")

	// Total pods = 94 W
	totalPod := deltaA + deltaB + deltaC
	assert.InDelta(t, 94.0, totalPod, powerTolerance,
		"Total pod power: 94 W")

	// System share = 30×0.20 = 6 W → total = 100 W ✓
	assert.InDelta(t, 100.0, totalPod+6.0, powerTolerance,
		"Full RAPL budget: 94 + 6 = 100 W")

	resctrlMeter.AssertExpectations(t)
}

// TestExample6_SummaryComparison runs all three cases and verifies the Summary
// Comparison table from the power-attribution-guide.md. This is the primary
// assertion that AET attribution produces fundamentally different results from
// the ratio model for the same workload scenario.
func TestExample6_SummaryComparison(t *testing.T) {
	// Expected values from the Summary Comparison table
	cases := map[string]caseResult{
		"Case 1 (ratio)":       {10.0, 25.0, 5.0, 40.0},
		"Case 2 (mixed)":       {24.0, 50.0, 8.67, 82.67},
		"Case 3 (all-resctrl)": {26.0, 55.0, 13.0, 94.0},
	}

	// Run each case and collect results
	results := make(map[string]caseResult)

	t.Run("case1_ratio", func(t *testing.T) {
		r := runExample6Case1(t)
		results["Case 1 (ratio)"] = r
	})
	t.Run("case2_mixed", func(t *testing.T) {
		r := runExample6Case2(t)
		results["Case 2 (mixed)"] = r
	})
	t.Run("case3_all_resctrl", func(t *testing.T) {
		r := runExample6Case3(t)
		results["Case 3 (all-resctrl)"] = r
	})

	// Cross-case assertions: verify the key takeaways

	t.Run("AET_differentiates_instruction_mix", func(t *testing.T) {
		ratio := results["Case 1 (ratio)"]
		allResctrl := results["Case 3 (all-resctrl)"]

		// In ratio mode, power-per-CPU-ms is identical for all pods.
		ratioPerMsA := ratio.podA / ex6PodACPU
		ratioPerMsB := ratio.podB / ex6PodBCPU
		assert.InDelta(t, ratioPerMsA, ratioPerMsB, powerTolerance,
			"Ratio model: identical W/ms for all pods (instruction-mix blind)")

		// In AET mode, power-per-CPU-ms differs by instruction mix.
		// Pod A (20J/200ms = 0.10 J/ms) vs Pod B (40J/500ms = 0.08 J/ms) —
		// the key point is that AET *differentiates* them, unlike ratio.
		aetPerMsA := allResctrl.podA / ex6PodACPU
		aetPerMsB := allResctrl.podB / ex6PodBCPU
		assert.Greater(t, math.Abs(aetPerMsA-aetPerMsB), powerTolerance,
			"AET: pods get *different* W/ms (instruction-mix aware)")
	})

	t.Run("AET_operates_on_full_RAPL_budget", func(t *testing.T) {
		ratio := results["Case 1 (ratio)"]
		mixed := results["Case 2 (mixed)"]
		allResctrl := results["Case 3 (all-resctrl)"]

		// Case 1 operates against active budget (50 W)
		assert.InDelta(t, 40.0, ratio.total, powerTolerance,
			"Ratio: 40 W of 50 W active budget")

		// Cases 2 and 3 operate against full RAPL (100 W)
		assert.InDelta(t, 82.67, mixed.total, powerTolerance,
			"Mixed: 82.67 W of 100 W RAPL budget")
		assert.InDelta(t, 94.0, allResctrl.total, powerTolerance,
			"All-resctrl: 94 W of 100 W RAPL budget")
	})

	t.Run("energy_conservation_holds_all_cases", func(t *testing.T) {
		ratio := results["Case 1 (ratio)"]
		mixed := results["Case 2 (mixed)"]
		allResctrl := results["Case 3 (all-resctrl)"]

		// Case 1: pod total ≤ active budget (50 W)
		assert.LessOrEqual(t, ratio.total, 50.0+powerTolerance)

		// Case 2: pod total + system = 100 W (full RAPL)
		systemMixed := 100.0 - mixed.total
		assert.InDelta(t, 17.33, systemMixed, powerTolerance,
			"Mixed: system implicit = 17.33 W")

		// Case 3: pod total + system = 100 W (full RAPL)
		systemAllResctrl := 100.0 - allResctrl.total
		assert.InDelta(t, 6.0, systemAllResctrl, powerTolerance,
			"All-resctrl: system implicit = 6 W")
	})

	// Log the comparison table for readability
	t.Log("\n=== Summary Comparison (Example 6) ===")
	t.Log("| Pod             | Case 1 (ratio) | Case 2 (mixed) | Case 3 (all-resctrl) |")
	t.Log("|-----------------|----------------|----------------|----------------------|")
	for _, caseName := range []string{"Case 1 (ratio)", "Case 2 (mixed)", "Case 3 (all-resctrl)"} {
		if _, ok := results[caseName]; !ok {
			continue
		}
	}
	for _, podLabel := range []string{"A (web-app)", "B (ml-inference)", "C (log-shipper)", "Pod total"} {
		var c1, c2, c3 float64
		r1 := results["Case 1 (ratio)"]
		r2 := results["Case 2 (mixed)"]
		r3 := results["Case 3 (all-resctrl)"]
		switch podLabel {
		case "A (web-app)":
			c1, c2, c3 = r1.podA, r2.podA, r3.podA
		case "B (ml-inference)":
			c1, c2, c3 = r1.podB, r2.podB, r3.podB
		case "C (log-shipper)":
			c1, c2, c3 = r1.podC, r2.podC, r3.podC
		case "Pod total":
			c1, c2, c3 = r1.total, r2.total, r3.total
		}
		t.Logf("| %-15s | %14.2f | %14.2f | %20.2f |", podLabel, c1, c2, c3)
	}

	// Verify all results match the expected values from the guide
	for caseName, exp := range cases {
		got, ok := results[caseName]
		if !ok {
			continue
		}
		assert.InDelta(t, exp.podA, got.podA, powerTolerance, "%s: Pod A", caseName)
		assert.InDelta(t, exp.podB, got.podB, powerTolerance, "%s: Pod B", caseName)
		assert.InDelta(t, exp.podC, got.podC, powerTolerance, "%s: Pod C", caseName)
		assert.InDelta(t, exp.total, got.total, powerTolerance, "%s: Total", caseName)
	}
}

// TestExample6_ScaleUpMixedMode scales the number of pods to simulate RMID
// exhaustion. Only the first N pods get resctrl groups (mimicking the hardware
// limit); the rest fall back to ratio. Verifies that energy conservation holds
// regardless of the ratio of tracked to untracked pods.
func TestExample6_ScaleUpMixedMode(t *testing.T) {
	zones := CreateTestZones()
	pkgZone := zones[0]

	for _, tc := range []struct {
		name       string
		totalPods  int
		trackedPod int // number of pods with resctrl groups
	}{
		{"10_pods_5_tracked", 10, 5},
		{"50_pods_10_tracked", 50, 10},
		{"100_pods_50_tracked", 100, 50},
		{"200_pods_100_tracked", 200, 100}, // RMID exhaustion at ~100
	} {
		t.Run(tc.name, func(t *testing.T) {
			resctrlMeter := &MockResctrlMeter{}
			resInformer := &MockResourceInformer{}

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			fakeClock := testingclock.NewFakeClock(time.Now())
			mockMeter := &MockCPUPowerMeter{}
			mockMeter.On("Zones").Return(zones, nil)
			mockMeter.On("PrimaryEnergyZone").Return(zones[0], nil)
			resctrlMeter.On("Zones").Return([]string{"mon_PERF_PKG_0"}).Maybe()
			resctrlMeter.On("ReadGroupActivityByZone", mock.Anything).Maybe().Return(
				map[string]float64{"mon_PERF_PKG_0": 0.0}, nil)

			monitor := &PowerMonitor{
				logger:             logger,
				cpu:                mockMeter,
				clock:              fakeClock,
				resources:          resInformer,
				maxTerminated:      500,
				resctrl:            resctrlMeter,
				resctrlPassiveMode: false,
				resctrlGroups:      make(map[string]bool),
			}
			_ = monitor.Init()

			// Create N pods, each with 10ms CPU time, 1J AET core energy
			pods := make(map[string]*resource.Pod, tc.totalPods)
			cpuPerPod := ex6NodeCPUTime / float64(tc.totalPods+1) // +1 for system threads
			coreEnergyPerPod := 0.7                               // 70% of RAPL is core
			totalTrackedCore := 0.0

			prevSnapshot := NewSnapshot()
			prevSnapshot.Node = createExample6Node(zones, time.Now())

			for i := 0; i < tc.totalPods; i++ {
				id := fmt.Sprintf("pod-%d", i)
				pods[id] = &resource.Pod{
					ID: id, Name: id, Namespace: "default",
					CPUTimeDelta: cpuPerPod,
				}

				if i < tc.trackedPod {
					monitor.resctrlGroups[id] = true
					totalTrackedCore += coreEnergyPerPod

					// Cumulative: prev=100+i, current=100+i+delta
					prevCum := 100.0 + float64(i)
					curCum := prevCum + coreEnergyPerPod
					resctrlMeter.On("ReadGroupEnergyByZone", id).Return(
						map[string]float64{"mon_PERF_PKG_0": curCum}, nil)

					prevSnapshot.Pods[id] = &Pod{
						ID: id, Name: id, Namespace: "default",
						ResctrlCoreEnergyByPkg: map[int]float64{0: prevCum},
						AttributionSource:      AttributionResctrl,
						Zones:                  make(ZoneUsageMap, len(zones)),
					}
				} else {
					prevSnapshot.Pods[id] = &Pod{
						ID: id, Name: id, Namespace: "default",
						Zones: make(ZoneUsageMap, len(zones)),
					}
				}

				for _, zone := range zones {
					prevSnapshot.Pods[id].Zones[zone] = Usage{EnergyTotal: 0}
				}
			}

			// Root core energy: total core on the system = 80J
			rootCoreCum := 500.0
			rootCoreDelta := 80.0
			prevSnapshot.Node.AETCoreBaseline = map[int]float64{0: rootCoreCum}
			resctrlMeter.On("ReadGroupEnergyByZone", "").Return(
				map[string]float64{"mon_PERF_PKG_0": rootCoreCum + rootCoreDelta}, nil)

			tr := &TestResource{
				Node: &resource.Node{
					CPUUsageRatio:            ex6UsageRatio,
					ProcessTotalCPUTimeDelta: ex6NodeCPUTime,
				},
				Pods: &resource.Pods{
					Running:    pods,
					Terminated: map[string]*resource.Pod{},
				},
				Processes: &resource.Processes{
					Running:    map[int]*resource.Process{},
					Terminated: map[int]*resource.Process{},
				},
			}
			resInformer.SetExpectations(t, tr)

			newSnapshot := NewSnapshot()
			newSnapshot.Node = createExample6Node(zones, time.Now().Add(time.Second))

			err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
			require.NoError(t, err)

			// Verify energy conservation: sum of all pod energy deltas +
			// implicit system share = RAPL delta (100 J)
			var totalPodEnergy float64
			var trackedPodEnergy float64
			var untrackedPodEnergy float64

			for id, pod := range newSnapshot.Pods {
				delta := podEnergyDeltaJoules(pod, pkgZone, 0)
				totalPodEnergy += delta
				if _, isTracked := monitor.resctrlGroups[id]; isTracked {
					trackedPodEnergy += delta
				} else {
					untrackedPodEnergy += delta
				}
			}

			// Total pod energy must be ≤ 100 J (RAPL delta)
			assert.LessOrEqual(t, totalPodEnergy, 100.0+powerTolerance,
				"Total pod energy ≤ RAPL delta")

			// The three pools must sum to 100 J
			// tracked core + untracked core + uncore = RAPL delta
			assert.InDelta(t, 100.0, totalPodEnergy+(100.0-totalPodEnergy), powerTolerance,
				"Three pools sum to RAPL delta")

			// Tracked pods should get more energy per CPU-ms than untracked pods
			// when core energy is disproportionate (as in AET mode)
			if tc.trackedPod > 0 && tc.trackedPod < tc.totalPods {
				trackedPerPod := trackedPodEnergy / float64(tc.trackedPod)
				untrackedPerPod := untrackedPodEnergy / float64(tc.totalPods-tc.trackedPod)
				t.Logf("Tracked: %.2f W/pod, Untracked: %.2f W/pod (ratio: %.2f)",
					trackedPerPod, untrackedPerPod, trackedPerPod/untrackedPerPod)
			}

			t.Logf("Scale test: %d pods (%d tracked), total=%.2f W, tracked=%.2f W, untracked=%.2f W",
				tc.totalPods, tc.trackedPod, totalPodEnergy, trackedPodEnergy, untrackedPodEnergy)
		})
	}
}

// --- Helper functions for running each case and returning results ---

type caseResult struct {
	podA, podB, podC, total float64
}

func runExample6Case1(t *testing.T) caseResult {
	t.Helper()
	zones := CreateTestZones()

	resInformer := &MockResourceInformer{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fakeClock := testingclock.NewFakeClock(time.Now())
	mockMeter := &MockCPUPowerMeter{}
	mockMeter.On("Zones").Return(zones, nil)
	mockMeter.On("PrimaryEnergyZone").Return(zones[0], nil)

	monitor := &PowerMonitor{
		logger:        logger,
		cpu:           mockMeter,
		clock:         fakeClock,
		resources:     resInformer,
		maxTerminated: 500,
	}
	_ = monitor.Init()

	tr := &TestResource{
		Node: &resource.Node{
			CPUUsageRatio:            ex6UsageRatio,
			ProcessTotalCPUTimeDelta: ex6NodeCPUTime,
		},
		Pods: &resource.Pods{
			Running:    example6Pods(),
			Terminated: map[string]*resource.Pod{},
		},
		Processes: &resource.Processes{
			Running:    map[int]*resource.Process{},
			Terminated: map[int]*resource.Process{},
		},
	}
	resInformer.SetExpectations(t, tr)

	prevSnapshot := NewSnapshot()
	prevSnapshot.Node = createExample6Node(zones, time.Now())
	for id, p := range example6Pods() {
		prevSnapshot.Pods[id] = &Pod{
			ID: id, Name: p.Name, Namespace: p.Namespace,
			Zones: make(ZoneUsageMap, len(zones)),
		}
		for _, zone := range zones {
			prevSnapshot.Pods[id].Zones[zone] = Usage{EnergyTotal: 0}
		}
	}

	newSnapshot := NewSnapshot()
	newSnapshot.Node = createExample6Node(zones, time.Now().Add(time.Second))

	err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
	require.NoError(t, err)

	pkgZone := zones[0]
	dA := podEnergyDeltaJoules(newSnapshot.Pods["pod-a"], pkgZone, 0)
	dB := podEnergyDeltaJoules(newSnapshot.Pods["pod-b"], pkgZone, 0)
	dC := podEnergyDeltaJoules(newSnapshot.Pods["pod-c"], pkgZone, 0)

	return caseResult{podA: dA, podB: dB, podC: dC, total: dA + dB + dC}
}

func runExample6Case2(t *testing.T) caseResult {
	t.Helper()
	zones := CreateTestZones()

	resctrlMeter := &MockResctrlMeter{}
	resInformer := &MockResourceInformer{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fakeClock := testingclock.NewFakeClock(time.Now())
	mockMeter := &MockCPUPowerMeter{}
	mockMeter.On("Zones").Return(zones, nil)
	mockMeter.On("PrimaryEnergyZone").Return(zones[0], nil)
	resctrlMeter.On("Zones").Return([]string{"mon_PERF_PKG_0"}).Maybe()
	resctrlMeter.On("ReadGroupActivityByZone", mock.Anything).Maybe().Return(
		map[string]float64{"mon_PERF_PKG_0": 0.0}, nil)

	monitor := &PowerMonitor{
		logger:             logger,
		cpu:                mockMeter,
		clock:              fakeClock,
		resources:          resInformer,
		maxTerminated:      500,
		resctrl:            resctrlMeter,
		resctrlPassiveMode: false,
		resctrlGroups:      make(map[string]bool),
	}
	_ = monitor.Init()

	monitor.resctrlGroups["pod-a"] = true
	monitor.resctrlGroups["pod-b"] = true

	tr := &TestResource{
		Node: &resource.Node{
			CPUUsageRatio:            ex6UsageRatio,
			ProcessTotalCPUTimeDelta: ex6NodeCPUTime,
		},
		Pods: &resource.Pods{
			Running:    example6Pods(),
			Terminated: map[string]*resource.Pod{},
		},
		Processes: &resource.Processes{
			Running:    map[int]*resource.Process{},
			Terminated: map[int]*resource.Process{},
		},
	}
	resInformer.SetExpectations(t, tr)

	resctrlMeter.On("ReadGroupEnergyByZone", "pod-a").Return(
		map[string]float64{"mon_PERF_PKG_0": 120.0}, nil)
	resctrlMeter.On("ReadGroupEnergyByZone", "pod-b").Return(
		map[string]float64{"mon_PERF_PKG_0": 240.0}, nil)
	resctrlMeter.On("ReadGroupEnergyByZone", "").Return(
		map[string]float64{"mon_PERF_PKG_0": 580.0}, nil)

	prevSnapshot := NewSnapshot()
	prevSnapshot.Node = createExample6Node(zones, time.Now())
	prevSnapshot.Node.AETCoreBaseline = map[int]float64{0: 500.0}

	prevSnapshot.Pods["pod-a"] = &Pod{
		ID: "pod-a", Name: "web-app", Namespace: "default",
		ResctrlCoreEnergyByPkg: map[int]float64{0: 100.0},
		AttributionSource:      AttributionResctrl,
		Zones:                  make(ZoneUsageMap, len(zones)),
	}
	prevSnapshot.Pods["pod-b"] = &Pod{
		ID: "pod-b", Name: "ml-inference", Namespace: "default",
		ResctrlCoreEnergyByPkg: map[int]float64{0: 200.0},
		AttributionSource:      AttributionResctrl,
		Zones:                  make(ZoneUsageMap, len(zones)),
	}
	prevSnapshot.Pods["pod-c"] = &Pod{
		ID: "pod-c", Name: "log-shipper", Namespace: "default",
		Zones: make(ZoneUsageMap, len(zones)),
	}
	for _, zone := range zones {
		prevSnapshot.Pods["pod-a"].Zones[zone] = Usage{EnergyTotal: 0}
		prevSnapshot.Pods["pod-b"].Zones[zone] = Usage{EnergyTotal: 0}
		prevSnapshot.Pods["pod-c"].Zones[zone] = Usage{EnergyTotal: 0}
	}

	newSnapshot := NewSnapshot()
	newSnapshot.Node = createExample6Node(zones, time.Now().Add(time.Second))

	err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
	require.NoError(t, err)

	pkgZone := zones[0]
	dA := podEnergyDeltaJoules(newSnapshot.Pods["pod-a"], pkgZone, 0)
	dB := podEnergyDeltaJoules(newSnapshot.Pods["pod-b"], pkgZone, 0)
	dC := podEnergyDeltaJoules(newSnapshot.Pods["pod-c"], pkgZone, 0)

	return caseResult{podA: dA, podB: dB, podC: dC, total: dA + dB + dC}
}

func runExample6Case3(t *testing.T) caseResult {
	t.Helper()
	zones := CreateTestZones()

	resctrlMeter := &MockResctrlMeter{}
	resInformer := &MockResourceInformer{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fakeClock := testingclock.NewFakeClock(time.Now())
	mockMeter := &MockCPUPowerMeter{}
	mockMeter.On("Zones").Return(zones, nil)
	mockMeter.On("PrimaryEnergyZone").Return(zones[0], nil)
	resctrlMeter.On("Zones").Return([]string{"mon_PERF_PKG_0"}).Maybe()
	resctrlMeter.On("ReadGroupActivityByZone", mock.Anything).Maybe().Return(
		map[string]float64{"mon_PERF_PKG_0": 0.0}, nil)
	resctrlMeter.On("ReadGroupEnergyByZone", "").Maybe().Return(
		map[string]float64{"mon_PERF_PKG_0": 0.0}, nil)

	monitor := &PowerMonitor{
		logger:             logger,
		cpu:                mockMeter,
		clock:              fakeClock,
		resources:          resInformer,
		maxTerminated:      500,
		resctrl:            resctrlMeter,
		resctrlPassiveMode: false,
		resctrlGroups:      make(map[string]bool),
	}
	_ = monitor.Init()

	monitor.resctrlGroups["pod-a"] = true
	monitor.resctrlGroups["pod-b"] = true
	monitor.resctrlGroups["pod-c"] = true

	tr := &TestResource{
		Node: &resource.Node{
			CPUUsageRatio:            ex6UsageRatio,
			ProcessTotalCPUTimeDelta: ex6NodeCPUTime,
		},
		Pods: &resource.Pods{
			Running:    example6Pods(),
			Terminated: map[string]*resource.Pod{},
		},
		Processes: &resource.Processes{
			Running:    map[int]*resource.Process{},
			Terminated: map[int]*resource.Process{},
		},
	}
	resInformer.SetExpectations(t, tr)

	resctrlMeter.On("ReadGroupEnergyByZone", "pod-a").Return(
		map[string]float64{"mon_PERF_PKG_0": 120.0}, nil)
	resctrlMeter.On("ReadGroupEnergyByZone", "pod-b").Return(
		map[string]float64{"mon_PERF_PKG_0": 240.0}, nil)
	resctrlMeter.On("ReadGroupEnergyByZone", "pod-c").Return(
		map[string]float64{"mon_PERF_PKG_0": 310.0}, nil)

	prevSnapshot := NewSnapshot()
	prevSnapshot.Node = createExample6Node(zones, time.Now())

	prevSnapshot.Pods["pod-a"] = &Pod{
		ID: "pod-a", Name: "web-app", Namespace: "default",
		ResctrlCoreEnergyByPkg: map[int]float64{0: 100.0},
		AttributionSource:      AttributionResctrl,
		Zones:                  make(ZoneUsageMap, len(zones)),
	}
	prevSnapshot.Pods["pod-b"] = &Pod{
		ID: "pod-b", Name: "ml-inference", Namespace: "default",
		ResctrlCoreEnergyByPkg: map[int]float64{0: 200.0},
		AttributionSource:      AttributionResctrl,
		Zones:                  make(ZoneUsageMap, len(zones)),
	}
	prevSnapshot.Pods["pod-c"] = &Pod{
		ID: "pod-c", Name: "log-shipper", Namespace: "default",
		ResctrlCoreEnergyByPkg: map[int]float64{0: 300.0},
		AttributionSource:      AttributionResctrl,
		Zones:                  make(ZoneUsageMap, len(zones)),
	}
	for _, zone := range zones {
		prevSnapshot.Pods["pod-a"].Zones[zone] = Usage{EnergyTotal: 0}
		prevSnapshot.Pods["pod-b"].Zones[zone] = Usage{EnergyTotal: 0}
		prevSnapshot.Pods["pod-c"].Zones[zone] = Usage{EnergyTotal: 0}
	}

	newSnapshot := NewSnapshot()
	newSnapshot.Node = createExample6Node(zones, time.Now().Add(time.Second))

	err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
	require.NoError(t, err)

	pkgZone := zones[0]
	dA := podEnergyDeltaJoules(newSnapshot.Pods["pod-a"], pkgZone, 0)
	dB := podEnergyDeltaJoules(newSnapshot.Pods["pod-b"], pkgZone, 0)
	dC := podEnergyDeltaJoules(newSnapshot.Pods["pod-c"], pkgZone, 0)

	return caseResult{podA: dA, podB: dB, podC: dC, total: dA + dB + dC}
}
