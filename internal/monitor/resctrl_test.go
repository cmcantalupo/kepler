// SPDX-FileCopyrightText: 2026 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/sustainable-computing-io/kepler/internal/device"
	"github.com/sustainable-computing-io/kepler/internal/resource"
	testingclock "k8s.io/utils/clock/testing"
)

// --- Mock ResctrlPowerMeter ---

type MockResctrlMeter struct {
	mock.Mock
}

func (m *MockResctrlMeter) Name() string { return "mock-resctrl" }

func (m *MockResctrlMeter) Init() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockResctrlMeter) Zones() []string {
	args := m.Called()
	return args.Get(0).([]string)
}

func (m *MockResctrlMeter) DiscoverGroups() (map[string][]int, error) {
	args := m.Called()
	return args.Get(0).(map[string][]int), args.Error(1)
}

func (m *MockResctrlMeter) CreateMonitorGroup(id string, pids []int) error {
	args := m.Called(id, pids)
	return args.Error(0)
}

func (m *MockResctrlMeter) DeleteMonitorGroup(id string) error {
	args := m.Called(id)
	return args.Error(0)
}

func (m *MockResctrlMeter) AddPIDsToGroup(id string, pids []int) error {
	args := m.Called(id, pids)
	return args.Error(0)
}

func (m *MockResctrlMeter) ReadGroupEnergy(id string) (float64, error) {
	args := m.Called(id)
	return args.Get(0).(float64), args.Error(1)
}

func (m *MockResctrlMeter) ReadGroupEnergyByZone(id string) (map[string]float64, error) {
	args := m.Called(id)
	return args.Get(0).(map[string]float64), args.Error(1)
}

func (m *MockResctrlMeter) GroupExists(id string) bool {
	args := m.Called(id)
	return args.Bool(0)
}

var _ device.ResctrlPowerMeter = (*MockResctrlMeter)(nil)

// --- Helper functions ---

// createMonitorWithResctrl builds a PowerMonitor with resctrl support for testing.
func createMonitorWithResctrl(
	zones []EnergyZone,
	resctrlMeter *MockResctrlMeter,
	resInformer *MockResourceInformer,
) *PowerMonitor {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fakeClock := testingclock.NewFakeClock(time.Now())

	mockMeter := &MockCPUPowerMeter{}
	mockMeter.On("Zones").Return(zones, nil)
	mockMeter.On("PrimaryEnergyZone").Return(zones[0], nil)
	// Setup resctrl mock expectations for Init()
	resctrlMeter.On("Zones").Return([]string{"mon_PERF_PKG_0"}).Maybe()
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
	return monitor
}

// createNodeSnapshotWithDelta creates a Node snapshot where package zones have a
// specific deltaEnergy value, used for testing uncore calculation.
func createNodeSnapshotWithDelta(zones []EnergyZone, timestamp time.Time, usageRatio float64, delta Energy) *Node {
	node := &Node{
		Timestamp:  timestamp,
		UsageRatio: usageRatio,
		Zones:      make(NodeZoneUsageMap),
	}
	// activeEnergy derives from delta, matching real code: activeEnergy = delta * usageRatio
	active := Energy(usageRatio * float64(delta))
	idle := delta - active
	power := Power(50 * Watt)
	activePower := Power(usageRatio * float64(power))
	idlePower := power - activePower
	for _, zone := range zones {
		node.Zones[zone] = NodeUsage{
			EnergyTotal:       200 * Joule,
			activeEnergy:      active,
			ActiveEnergyTotal: active,
			IdleEnergyTotal:   idle,
			Power:             power,
			ActivePower:       activePower,
			IdlePower:         idlePower,
			deltaEnergy:       delta,
		}
	}
	return node
}

// --- Tests ---

func TestHybridPodPowerAttribution(t *testing.T) {
	zones := CreateTestZones() // package-0 (index 0) and core-0 (index 1)

	t.Run("resctrl_pod_gets_direct_core_energy", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)

		pod1 := &resource.Pod{
			ID:           "pod-1",
			Name:         "resctrl-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
		}

		tr := &TestResource{
			Node: &resource.Node{
				CPUUsageRatio:            0.5,
				ProcessTotalCPUTimeDelta: 200.0,
			},
			Pods: &resource.Pods{
				Running:    map[string]*resource.Pod{"pod-1": pod1},
				Terminated: map[string]*resource.Pod{},
			},
			Processes: &resource.Processes{
				Running:    map[int]*resource.Process{},
				Terminated: map[int]*resource.Process{},
			},
		}
		resInformer.SetExpectations(t, tr)

		// Mark pod-1 as having a resctrl group
		monitor.resctrlGroups["pod-1"] = true

		// Resctrl returns 200J cumulative core energy on package 0
		resctrlMeter.On("ReadGroupEnergyByZone", "pod-1").Return(map[string]float64{"mon_PERF_PKG_0": 200.0}, nil)

		// Previous snapshot with pod-1 having 100J resctrl cumulative on pkg 0
		prevSnapshot := NewSnapshot()
		prevSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now(), 0.5, 100*Joule)
		prevSnapshot.Pods["pod-1"] = &Pod{
			ID:                     "pod-1",
			Name:                   "resctrl-pod",
			Namespace:              "default",
			CPUTotalTime:           5.0,
			ResctrlCoreEnergyByPkg: map[int]float64{0: 100.0},
			AttributionSource:      AttributionResctrl,
			Zones:                  make(ZoneUsageMap, len(zones)),
		}
		for _, zone := range zones {
			prevSnapshot.Pods["pod-1"].Zones[zone] = Usage{
				EnergyTotal: 25 * Joule,
				Power:       Power(0),
			}
		}

		newSnapshot := NewSnapshot()
		newSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now().Add(time.Second), 0.5, 100*Joule)

		err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
		require.NoError(t, err)

		pod := newSnapshot.Pods["pod-1"]
		require.NotNil(t, pod, "pod-1 should exist in snapshot")

		// Attribution source should be resctrl
		assert.Equal(t, AttributionResctrl, pod.AttributionSource)
		// Cumulative counter should be updated (per-package)
		assert.Equal(t, 200.0, pod.ResctrlCoreEnergyByPkg[0])

		// All-resctrl mode: TotalResctrlCoreEnergyByPkg stores raw AET deltas (no UsageRatio scaling).
		// Raw delta = 200-100 = 100J.
		assert.Equal(t, 100.0, newSnapshot.TotalResctrlCoreEnergyByPkg[0])

		// For the package zone (package-0): raw core delta = 100J
		// deltaEnergy(node) = 100J, totalCore = 100J
		// uncore = max(100J - 100J, 0) = 0, normFactor = 1.0
		// Pod energy = 100J (core) + 0 (uncore) = 100J delta + 25J prev
		pkgZone := zones[0] // package-0
		pkgUsage := pod.Zones[pkgZone]
		assert.Equal(t, Energy(125*Joule), pkgUsage.EnergyTotal,
			"Package zone: raw core delta (100J) + prev (25J) = 125J")

		// For core-0 (not a package zone): should use ratio model
		coreZone := zones[1] // core-0
		coreUsage := pod.Zones[coreZone]
		// ratio = 100/200 = 0.5, activeEnergy = 0.5 * 50J = 25J delta
		assert.Equal(t, Energy(50*Joule), coreUsage.EnergyTotal,
			"Core zone: ratio-based (25J delta + 25J prev = 50J)")

		resctrlMeter.AssertExpectations(t)
	})

	t.Run("all_resctrl_uses_raw_deltas_against_total_rapl", func(t *testing.T) {
		// When every running pod has a resctrl group, the optimized all-resctrl
		// path activates: raw AET deltas are used (no UsageRatio scaling) and the
		// uncore budget is derived from RAPL deltaEnergy (total, not activeEnergy).
		// This preserves hardware-measured core fidelity.
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)

		// Two pods with very different AET core energy: dgemm pod = 80J, idle pod = 10J
		podHeavy := &resource.Pod{
			ID:           "pod-heavy",
			Name:         "dgemm-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
		}
		podLight := &resource.Pod{
			ID:           "pod-light",
			Name:         "idle-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
		}

		tr := &TestResource{
			Node: &resource.Node{
				CPUUsageRatio:            0.5,
				ProcessTotalCPUTimeDelta: 200.0,
			},
			Pods: &resource.Pods{
				Running: map[string]*resource.Pod{
					"pod-heavy": podHeavy,
					"pod-light": podLight,
				},
				Terminated: map[string]*resource.Pod{},
			},
			Processes: &resource.Processes{
				Running:    map[int]*resource.Process{},
				Terminated: map[int]*resource.Process{},
			},
		}
		resInformer.SetExpectations(t, tr)

		monitor.resctrlGroups["pod-heavy"] = true
		monitor.resctrlGroups["pod-light"] = true

		// AET: heavy pod 180J cumulative, light pod 110J cumulative
		resctrlMeter.On("ReadGroupEnergyByZone", "pod-heavy").Return(
			map[string]float64{"mon_PERF_PKG_0": 180.0}, nil)
		resctrlMeter.On("ReadGroupEnergyByZone", "pod-light").Return(
			map[string]float64{"mon_PERF_PKG_0": 110.0}, nil)

		prevSnapshot := NewSnapshot()
		prevSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now(), 0.5, 100*Joule)
		prevSnapshot.Pods["pod-heavy"] = &Pod{
			ID:                     "pod-heavy",
			Name:                   "dgemm-pod",
			Namespace:              "default",
			ResctrlCoreEnergyByPkg: map[int]float64{0: 100.0}, // delta = 180-100 = 80J
			AttributionSource:      AttributionResctrl,
			Zones:                  make(ZoneUsageMap, len(zones)),
		}
		prevSnapshot.Pods["pod-light"] = &Pod{
			ID:                     "pod-light",
			Name:                   "idle-pod",
			Namespace:              "default",
			ResctrlCoreEnergyByPkg: map[int]float64{0: 100.0}, // delta = 110-100 = 10J
			AttributionSource:      AttributionResctrl,
			Zones:                  make(ZoneUsageMap, len(zones)),
		}
		for _, zone := range zones {
			prevSnapshot.Pods["pod-heavy"].Zones[zone] = Usage{EnergyTotal: 0}
			prevSnapshot.Pods["pod-light"].Zones[zone] = Usage{EnergyTotal: 0}
		}

		newSnapshot := NewSnapshot()
		newSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now().Add(time.Second), 0.5, 100*Joule)

		err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
		require.NoError(t, err)

		heavy := newSnapshot.Pods["pod-heavy"]
		light := newSnapshot.Pods["pod-light"]
		require.NotNil(t, heavy)
		require.NotNil(t, light)

		// Raw AET deltas: heavy=80J, light=10J, total=90J
		assert.Equal(t, 90.0, newSnapshot.TotalResctrlCoreEnergyByPkg[0])

		// All-resctrl budget: deltaEnergy=100J, totalCore=90J
		// uncore = 100-90 = 10J (residual: true uncore + idle + system overhead)
		// normFactor = 1.0 (core < total)
		//
		// Heavy pod: core=80J + uncoreShare=10*0.5=5J = 85J
		// Light pod: core=10J + uncoreShare=10*0.5=5J = 15J
		// Sum = 85 + 15 = 100J = deltaEnergy ✓
		//
		// Compare to mixed-mode result (if UsageRatio scaling were used):
		//   scaledCore: heavy=40J, light=5J, total=45J
		//   activeEnergy=50J, uncore=5J
		//   Heavy: 40+2.5=42.5J, Light: 5+2.5=7.5J — diluted by the linear approximation.
		pkgZone := zones[0]
		assert.Equal(t, Energy(85*Joule), heavy.Zones[pkgZone].EnergyTotal,
			"Heavy pod: 80J raw core + 5J residual share = 85J")
		assert.Equal(t, Energy(15*Joule), light.Zones[pkgZone].EnergyTotal,
			"Light pod: 10J raw core + 5J residual share = 15J")

		// Conservation: sum of all pod energy = RAPL total delta
		totalPodEnergy := heavy.Zones[pkgZone].EnergyTotal + light.Zones[pkgZone].EnergyTotal
		assert.Equal(t, Energy(100*Joule), totalPodEnergy,
			"All-resctrl: sum(pod energy) = RAPL deltaEnergy = 100J")

		// Non-package zone (core-0): still uses ratio model
		coreZone := zones[1]
		// ratio = 100/200 = 0.5, activeEnergy = 0.5*50J = 25J
		assert.Equal(t, Energy(25*Joule), heavy.Zones[coreZone].EnergyTotal)
		assert.Equal(t, Energy(25*Joule), light.Zones[coreZone].EnergyTotal)

		resctrlMeter.AssertExpectations(t)
	})

	t.Run("non_resctrl_pod_uses_ratio_model", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)

		pod1 := &resource.Pod{
			ID:           "pod-1",
			Name:         "ratio-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
		}

		tr := &TestResource{
			Node: &resource.Node{
				CPUUsageRatio:            0.5,
				ProcessTotalCPUTimeDelta: 200.0,
			},
			Pods: &resource.Pods{
				Running:    map[string]*resource.Pod{"pod-1": pod1},
				Terminated: map[string]*resource.Pod{},
			},
			Processes: &resource.Processes{
				Running:    map[int]*resource.Process{},
				Terminated: map[int]*resource.Process{},
			},
		}
		resInformer.SetExpectations(t, tr)

		// No resctrl groups — pod-1 is NOT in resctrlGroups

		prevSnapshot := NewSnapshot()
		prevSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now(), 0.5, 100*Joule)
		prevSnapshot.Pods["pod-1"] = &Pod{
			ID:           "pod-1",
			Name:         "ratio-pod",
			Namespace:    "default",
			CPUTotalTime: 5.0,
			Zones:        make(ZoneUsageMap, len(zones)),
		}
		for _, zone := range zones {
			prevSnapshot.Pods["pod-1"].Zones[zone] = Usage{
				EnergyTotal: 25 * Joule,
				Power:       Power(0),
			}
		}

		newSnapshot := NewSnapshot()
		newSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now().Add(time.Second), 0.5, 100*Joule)

		err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
		require.NoError(t, err)

		pod := newSnapshot.Pods["pod-1"]
		require.NotNil(t, pod)
		assert.Equal(t, AttributionRatio, pod.AttributionSource)
		assert.Nil(t, pod.ResctrlCoreEnergyByPkg, "Non-resctrl pod has nil resctrl energy map")

		// Both zones should use ratio: 100/200 = 0.5, activeEnergy = 0.5*50J = 25J delta + 25J prev = 50J
		for _, zone := range zones {
			usage := pod.Zones[zone]
			assert.Equal(t, Energy(50*Joule), usage.EnergyTotal,
				"Zone %s: ratio model 25J delta + 25J prev = 50J", zone.Name())
		}
	})

	t.Run("mixed_resctrl_and_ratio_pods", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)

		pod1 := &resource.Pod{
			ID:           "pod-resctrl",
			Name:         "resctrl-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
		}
		pod2 := &resource.Pod{
			ID:           "pod-ratio",
			Name:         "ratio-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
		}

		tr := &TestResource{
			Node: &resource.Node{
				CPUUsageRatio:            0.5,
				ProcessTotalCPUTimeDelta: 200.0,
			},
			Pods: &resource.Pods{
				Running: map[string]*resource.Pod{
					"pod-resctrl": pod1,
					"pod-ratio":   pod2,
				},
				Terminated: map[string]*resource.Pod{},
			},
			Processes: &resource.Processes{
				Running:    map[int]*resource.Process{},
				Terminated: map[int]*resource.Process{},
			},
		}
		resInformer.SetExpectations(t, tr)

		monitor.resctrlGroups["pod-resctrl"] = true
		resctrlMeter.On("ReadGroupEnergyByZone", "pod-resctrl").Return(map[string]float64{"mon_PERF_PKG_0": 300.0}, nil)

		prevSnapshot := NewSnapshot()
		prevSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now(), 0.5, 100*Joule)
		prevSnapshot.Pods["pod-resctrl"] = &Pod{
			ID:                     "pod-resctrl",
			Name:                   "resctrl-pod",
			Namespace:              "default",
			ResctrlCoreEnergyByPkg: map[int]float64{0: 250.0},
			AttributionSource:      AttributionResctrl,
			Zones:                  make(ZoneUsageMap, len(zones)),
		}
		prevSnapshot.Pods["pod-ratio"] = &Pod{
			ID:        "pod-ratio",
			Name:      "ratio-pod",
			Namespace: "default",
			Zones:     make(ZoneUsageMap, len(zones)),
		}
		for _, zone := range zones {
			prevSnapshot.Pods["pod-resctrl"].Zones[zone] = Usage{EnergyTotal: 10 * Joule}
			prevSnapshot.Pods["pod-ratio"].Zones[zone] = Usage{EnergyTotal: 10 * Joule}
		}

		newSnapshot := NewSnapshot()
		newSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now().Add(time.Second), 0.5, 100*Joule)

		err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
		require.NoError(t, err)

		resctrlPod := newSnapshot.Pods["pod-resctrl"]
		ratioPod := newSnapshot.Pods["pod-ratio"]

		assert.Equal(t, AttributionResctrl, resctrlPod.AttributionSource)
		assert.Equal(t, AttributionRatio, ratioPod.AttributionSource)

		// Core delta for resctrl pod = 300 - 250 = 50J raw, active = 50 * 0.5 = 25J
		assert.Equal(t, 25.0, newSnapshot.TotalResctrlCoreEnergyByPkg[0])

		// Phase 2: raplActive = 50J, totalCore = 25J → uncore = 25J, normFactor = 1.0
		// Resctrl pod (package-0): normCore=25J + uncoreShare=25*0.5=12.5J = 37.5J + 10J prev
		pkgZone := zones[0]
		assert.Equal(t, Energy(95*Joule/2), resctrlPod.Zones[pkgZone].EnergyTotal,
			"Resctrl pod: 25J core + 12.5J uncore share + 10J prev = 47.5J")

		// Ratio pod (package-0): only gets uncore share = 25J * 0.5 = 12.5J + 10J prev
		assert.Equal(t, Energy(45*Joule/2), ratioPod.Zones[pkgZone].EnergyTotal,
			"Ratio pod on package zone: 12.5J uncore share + 10J prev = 22.5J")

		// Conservation: 37.5J + 12.5J = 50J = raplActive ✓
		// (both pods have cpuTimeRatio = 100/200 = 0.5)

		// Non-package zone (core-0): both pods use pure ratio model
		coreZone := zones[1]
		// ratio = 0.5, activeEnergy = 0.5 * 50J = 25J + 10J prev = 35J
		assert.Equal(t, Energy(35*Joule), resctrlPod.Zones[coreZone].EnergyTotal,
			"Resctrl pod on non-pkg zone: ratio model 25J + 10J prev = 35J")
		assert.Equal(t, Energy(35*Joule), ratioPod.Zones[coreZone].EnergyTotal,
			"Ratio pod on non-pkg zone: ratio model 25J + 10J prev = 35J")

		resctrlMeter.AssertExpectations(t)
	})

	t.Run("uncore_clamped_to_zero_when_core_exceeds_package", func(t *testing.T) {
		// If resctrl reports more core energy than the RAPL package delta
		// (possible due to measurement timing), uncore should be 0, not negative.
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)

		pod1 := &resource.Pod{
			ID:           "pod-1",
			Name:         "hot-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
		}

		tr := &TestResource{
			Node: &resource.Node{
				CPUUsageRatio:            0.5,
				ProcessTotalCPUTimeDelta: 200.0,
			},
			Pods: &resource.Pods{
				Running:    map[string]*resource.Pod{"pod-1": pod1},
				Terminated: map[string]*resource.Pod{},
			},
			Processes: &resource.Processes{
				Running:    map[int]*resource.Process{},
				Terminated: map[int]*resource.Process{},
			},
		}
		resInformer.SetExpectations(t, tr)

		monitor.resctrlGroups["pod-1"] = true
		// Core delta = 150J, but package deltaEnergy is only 100J
		resctrlMeter.On("ReadGroupEnergyByZone", "pod-1").Return(map[string]float64{"mon_PERF_PKG_0": 250.0}, nil)

		prevSnapshot := NewSnapshot()
		prevSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now(), 0.5, 100*Joule)
		prevSnapshot.Pods["pod-1"] = &Pod{
			ID:                     "pod-1",
			Name:                   "hot-pod",
			Namespace:              "default",
			ResctrlCoreEnergyByPkg: map[int]float64{0: 100.0}, // delta = 250 - 100 = 150J (exceeds pkg delta of 100J)
			AttributionSource:      AttributionResctrl,
			Zones:                  make(ZoneUsageMap, len(zones)),
		}
		for _, zone := range zones {
			prevSnapshot.Pods["pod-1"].Zones[zone] = Usage{EnergyTotal: 10 * Joule}
		}

		newSnapshot := NewSnapshot()
		newSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now().Add(time.Second), 0.5, 100*Joule)

		err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
		require.NoError(t, err)

		pod := newSnapshot.Pods["pod-1"]
		require.NotNil(t, pod)

		// All-resctrl mode: raw core delta = 150J (no UsageRatio scaling)
		// deltaEnergy(node) = 100J, totalCore = 150J
		// AET core (150J) > RAPL total (100J) → normFactor = 100/150 ≈ 0.667
		// normCore = 150 * (100/150) = 100J, uncore = 0
		// Total pod = 100J (normalized core) + 0 (uncore) + 10J prev = 110J
		pkgZone := zones[0]
		assert.Equal(t, Energy(110*Joule), pod.Zones[pkgZone].EnergyTotal,
			"When core > pkg total, normalization ensures conservation: 100J + 10J prev = 110J")
	})

	t.Run("firstPodRead_seeds_resctrl_baseline", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)

		pod1 := &resource.Pod{
			ID:           "pod-1",
			Name:         "seed-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
			CPUTotalTime: 100.0,
		}

		tr := &TestResource{
			Node: &resource.Node{
				CPUUsageRatio:            0.5,
				ProcessTotalCPUTimeDelta: 200.0,
			},
			Pods: &resource.Pods{
				Running:    map[string]*resource.Pod{"pod-1": pod1},
				Terminated: map[string]*resource.Pod{},
			},
			Processes: &resource.Processes{
				Running:    map[int]*resource.Process{},
				Terminated: map[int]*resource.Process{},
			},
		}
		resInformer.SetExpectations(t, tr)

		monitor.resctrlGroups["pod-1"] = true
		resctrlMeter.On("ReadGroupEnergyByZone", "pod-1").Return(map[string]float64{"mon_PERF_PKG_0": 500.0}, nil)

		snapshot := NewSnapshot()
		snapshot.Node = createNodeSnapshotWithDelta(zones, time.Now(), 0.5, 100*Joule)

		err := monitor.firstPodRead(snapshot)
		require.NoError(t, err)

		pod := snapshot.Pods["pod-1"]
		require.NotNil(t, pod)

		assert.Equal(t, 500.0, pod.ResctrlCoreEnergyByPkg[0],
			"First read should seed the cumulative resctrl counter for pkg 0")
		// Seed-only cycle: no AET delta yet, so attribution stays ratio.
		assert.Equal(t, AttributionRatio, pod.AttributionSource)

		resctrlMeter.AssertExpectations(t)
	})

	t.Run("no_resctrl_meter_uses_pure_ratio", func(t *testing.T) {
		// When resctrl is nil (not configured), everything should use ratio model
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
			// resctrl is nil
		}
		_ = monitor.Init()

		pod1 := &resource.Pod{
			ID:           "pod-1",
			Name:         "simple-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
		}

		tr := &TestResource{
			Node: &resource.Node{
				CPUUsageRatio:            0.5,
				ProcessTotalCPUTimeDelta: 200.0,
			},
			Pods: &resource.Pods{
				Running:    map[string]*resource.Pod{"pod-1": pod1},
				Terminated: map[string]*resource.Pod{},
			},
			Processes: &resource.Processes{
				Running:    map[int]*resource.Process{},
				Terminated: map[int]*resource.Process{},
			},
		}
		resInformer.SetExpectations(t, tr)

		prevSnapshot := NewSnapshot()
		prevSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now(), 0.5, 100*Joule)

		newSnapshot := NewSnapshot()
		newSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now().Add(time.Second), 0.5, 100*Joule)

		err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
		require.NoError(t, err)

		pod := newSnapshot.Pods["pod-1"]
		require.NotNil(t, pod)
		assert.Equal(t, AttributionRatio, pod.AttributionSource)
		assert.Empty(t, newSnapshot.TotalResctrlCoreEnergyByPkg,
			"No resctrl → empty total resctrl core energy map")
	})

	t.Run("aggregated_rapl_zone_sums_all_aet_packages", func(t *testing.T) {
		// Simulates systems (e.g., multi-tile single-socket) where RAPL
		// reports a single "package" zone without a numeric index, while AET
		// reports per-tile zones (mon_PERF_PKG_00, mon_PERF_PKG_01).
		aggregatedZones := []EnergyZone{
			device.NewMockRaplZone("package", 0, "/sys/class/powercap/intel-rapl/intel-rapl:0", 1000*Joule),
			device.NewMockRaplZone("dram", 0, "/sys/class/powercap/intel-rapl/intel-rapl:0:0", 500*Joule),
		}

		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(aggregatedZones, resctrlMeter, resInformer)

		pod1 := &resource.Pod{
			ID:           "pod-1",
			Name:         "multi-tile-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
		}

		tr := &TestResource{
			Node: &resource.Node{
				CPUUsageRatio:            0.5,
				ProcessTotalCPUTimeDelta: 200.0,
			},
			Pods: &resource.Pods{
				Running:    map[string]*resource.Pod{"pod-1": pod1},
				Terminated: map[string]*resource.Pod{},
			},
			Processes: &resource.Processes{
				Running:    map[int]*resource.Process{},
				Terminated: map[int]*resource.Process{},
			},
		}
		resInformer.SetExpectations(t, tr)

		monitor.resctrlGroups["pod-1"] = true
		// AET returns per-tile energy: 90J on tile 0, 70J on tile 1 (cumulative)
		resctrlMeter.On("ReadGroupEnergyByZone", "pod-1").Return(
			map[string]float64{"mon_PERF_PKG_00": 90.0, "mon_PERF_PKG_01": 70.0}, nil)

		// Previous snapshot: pod had 50J on pkg 0, 30J on pkg 1
		prevSnapshot := NewSnapshot()
		prevSnapshot.Node = createNodeSnapshotWithDelta(aggregatedZones, time.Now(), 0.5, 200*Joule)
		prevSnapshot.Pods["pod-1"] = &Pod{
			ID:                     "pod-1",
			Name:                   "multi-tile-pod",
			Namespace:              "default",
			ResctrlCoreEnergyByPkg: map[int]float64{0: 50.0, 1: 30.0},
			AttributionSource:      AttributionResctrl,
			Zones:                  make(ZoneUsageMap, len(aggregatedZones)),
		}
		for _, zone := range aggregatedZones {
			prevSnapshot.Pods["pod-1"].Zones[zone] = Usage{EnergyTotal: 10 * Joule}
		}

		newSnapshot := NewSnapshot()
		newSnapshot.Node = createNodeSnapshotWithDelta(aggregatedZones, time.Now().Add(time.Second), 0.5, 200*Joule)

		err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
		require.NoError(t, err)

		pod := newSnapshot.Pods["pod-1"]
		require.NotNil(t, pod)
		assert.Equal(t, AttributionResctrl, pod.AttributionSource)

		// All-resctrl mode: raw core deltas (no UsageRatio scaling)
		// pkg0 = 90-50 = 40J, pkg1 = 70-30 = 40J
		assert.Equal(t, 40.0, newSnapshot.TotalResctrlCoreEnergyByPkg[0])
		assert.Equal(t, 40.0, newSnapshot.TotalResctrlCoreEnergyByPkg[1])

		// Package zone ("package" with no index):
		// deltaEnergy = 200J, totalCore = 40+40 = 80J
		// uncore = max(200J - 80J, 0) = 120J, normFactor = 1.0
		// Pod core delta = sum(40+40) = 80J (aggregated across tiles)
		// Pod uncore share = 120J * 0.5 (cpuTimeRatio) = 60J
		// podEnergy = (80 + 60) * Joule = 140J
		// absoluteEnergy = 140J + 10J (prev) = 150J
		pkgZone := aggregatedZones[0]
		assert.Equal(t, Energy(150*Joule), pod.Zones[pkgZone].EnergyTotal,
			"Aggregated RAPL zone: 80J raw core + 60J uncore share + 10J prev = 150J")

		// activePower = (140J / 100J activeEnergy) * 25W ActivePower = 35W
		// (exceeds node active power because all-resctrl uses deltaEnergy not activeEnergy)
		assert.Equal(t, Power(35*Watt), pod.Zones[pkgZone].Power,
			"Package power reflects all-resctrl attribution")

		// DRAM zone (not a package zone): uses ratio model
		// ratio = 100/200 = 0.5, activeEnergy = 0.5 * 100J = 50J delta + 10J prev = 60J
		dramZone := aggregatedZones[1]
		assert.Equal(t, Energy(60*Joule), pod.Zones[dramZone].EnergyTotal,
			"DRAM zone uses ratio model: 50J delta + 10J prev = 60J")

		resctrlMeter.AssertExpectations(t)
	})

	t.Run("transient_read_failure_preserves_baseline", func(t *testing.T) {
		// When ReadGroupEnergyByZone fails for a pod with a tracked resctrl group,
		// the baseline cumulative counters must be preserved from the previous snapshot
		// so that future deltas remain correct once reads succeed again.
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)

		pod1 := &resource.Pod{
			ID:           "pod-1",
			Name:         "resctrl-pod",
			Namespace:    "default",
			CPUTimeDelta: 100.0,
		}

		tr := &TestResource{
			Node: &resource.Node{
				CPUUsageRatio:            0.5,
				ProcessTotalCPUTimeDelta: 200.0,
			},
			Pods: &resource.Pods{
				Running:    map[string]*resource.Pod{"pod-1": pod1},
				Terminated: map[string]*resource.Pod{},
			},
			Processes: &resource.Processes{
				Running:    map[int]*resource.Process{},
				Terminated: map[int]*resource.Process{},
			},
		}
		resInformer.SetExpectations(t, tr)

		monitor.resctrlGroups["pod-1"] = true

		// Simulate a transient read failure
		resctrlMeter.On("ReadGroupEnergyByZone", "pod-1").Return(
			map[string]float64(nil), fmt.Errorf("transient I/O error"))

		// Previous snapshot with pod-1 having 100J resctrl cumulative on pkg 0
		prevSnapshot := NewSnapshot()
		prevSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now(), 0.5, 100*Joule)
		prevSnapshot.Pods["pod-1"] = &Pod{
			ID:                     "pod-1",
			Name:                   "resctrl-pod",
			Namespace:              "default",
			CPUTotalTime:           5.0,
			ResctrlCoreEnergyByPkg: map[int]float64{0: 100.0},
			AttributionSource:      AttributionResctrl,
			Zones:                  make(ZoneUsageMap, len(zones)),
		}
		for _, zone := range zones {
			prevSnapshot.Pods["pod-1"].Zones[zone] = Usage{
				EnergyTotal: 25 * Joule,
				Power:       Power(0),
			}
		}

		newSnapshot := NewSnapshot()
		newSnapshot.Node = createNodeSnapshotWithDelta(zones, time.Now().Add(time.Second), 0.5, 100*Joule)

		err := monitor.calculatePodPower(prevSnapshot, newSnapshot)
		require.NoError(t, err)

		pod := newSnapshot.Pods["pod-1"]
		require.NotNil(t, pod, "pod should still exist in snapshot after read failure")

		// The baseline cumulative counters must be preserved from previous snapshot
		assert.Equal(t, map[int]float64{0: 100.0}, pod.ResctrlCoreEnergyByPkg,
			"Previous cumulative counters should be carried forward on read failure")

		// The carried-forward map must be a deep copy, not a shared reference,
		// to preserve snapshot immutability. Mutating the new snapshot's map
		// must not affect the previous snapshot's map.
		prevMap := prevSnapshot.Pods["pod-1"].ResctrlCoreEnergyByPkg
		pod.ResctrlCoreEnergyByPkg[1] = 200.0
		assert.Equal(t, map[int]float64{0: 100.0}, prevMap,
			"Previous snapshot's map should remain unchanged when new snapshot's map is mutated")

		// Pod falls back to ratio attribution for this cycle
		assert.Equal(t, AttributionRatio, pod.AttributionSource,
			"Attribution should fall back to ratio on transient failure")

		resctrlMeter.AssertExpectations(t)
	})
}

func TestIsPackageZone(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"package-0", true},
		{"package-1", true},
		{"package", true}, // aggregated (no index) — multi-tile single-socket
		{"Package-0", true},
		{"PACKAGE-0", true},
		{"psys", true},
		{"PSYS", true},
		{"core-0", false},
		{"dram-0", false},
		{"dram", false},
		{"uncore-0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zone := device.NewMockRaplZone(tt.name, 0, "/test", 0)
			assert.Equal(t, tt.expected, isPackageZone(zone),
				"isPackageZone(%q) should be %v", tt.name, tt.expected)
		})
	}
}

func TestResctrlGroupManagement(t *testing.T) {
	zones := CreateTestZones()

	t.Run("passive_mode_discovers_groups", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)
		monitor.resctrlPassiveMode = true

		pod1 := &resource.Pod{
			ID:        "abcd-1234-uuid",
			Name:      "pod-1",
			Namespace: "default",
		}

		pods := &resource.Pods{
			Running: map[string]*resource.Pod{
				"abcd-1234-uuid": pod1,
			},
			Terminated: map[string]*resource.Pod{},
		}
		resInformer.On("Pods").Return(pods)

		// Resctrl discovers groups including the pod's UUID and an unmatched one
		discovered := map[string][]int{
			"abcd-1234-uuid": {100, 200},
			"stale-uuid":     {300},
		}
		resctrlMeter.On("DiscoverGroups").Return(discovered, nil)

		monitor.manageResctrlGroups()

		assert.True(t, monitor.resctrlGroups["abcd-1234-uuid"],
			"Discovered group matching running pod should be tracked")
		assert.False(t, monitor.resctrlGroups["stale-uuid"],
			"Discovered group not matching a running pod should be ignored")

		resctrlMeter.AssertExpectations(t)
	})

	t.Run("active_mode_creates_and_deletes_groups", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)
		monitor.resctrlPassiveMode = false
		monitor.resctrlReconciled = true // not testing startup reconciliation

		// pod1 is new (needs group created), pod-old is terminated
		pod1 := &resource.Pod{ID: "pod-new"}
		container := &resource.Container{
			ID:  "c1",
			Pod: pod1,
		}
		_ = container

		pods := &resource.Pods{
			Running: map[string]*resource.Pod{
				"pod-new": pod1,
			},
			Terminated: map[string]*resource.Pod{
				"pod-old": {ID: "pod-old"},
			},
		}
		resInformer.On("Pods").Return(pods)

		// Seed an existing tracked group for the terminated pod
		monitor.resctrlGroups["pod-old"] = true

		// Process for the new pod — note: proc.Container does NOT have .Pod set
		// (matching real resource informer behavior). The pod linkage comes
		// from containers.Running where .Pod IS set.
		processes := &resource.Processes{
			Running: map[int]*resource.Process{
				42: {
					PID:       42,
					Container: &resource.Container{ID: "c1"},
				},
			},
			Terminated: map[int]*resource.Process{},
		}
		resInformer.On("Processes").Return(processes)

		// Containers have .Pod set (this is where the informer sets it)
		containers := &resource.Containers{
			Running: map[string]*resource.Container{
				"c1": {ID: "c1", Pod: pod1},
			},
			Terminated: map[string]*resource.Container{},
		}
		resInformer.On("Containers").Return(containers)

		resctrlMeter.On("DeleteMonitorGroup", "pod-old").Return(nil)
		resctrlMeter.On("CreateMonitorGroup", "pod-new", []int{42}).Return(nil)

		monitor.manageResctrlGroups()

		assert.True(t, monitor.resctrlGroups["pod-new"],
			"New pod should have a tracked group")
		assert.False(t, monitor.resctrlGroups["pod-old"],
			"Terminated pod's group should be deleted")

		resctrlMeter.AssertExpectations(t)
	})

	t.Run("active_mode_resyncs_pids_for_existing_groups", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)
		monitor.resctrlPassiveMode = false
		monitor.resctrlReconciled = true // not testing startup reconciliation

		// pod-existing is already tracked (e.g., created on previous sync)
		pod := &resource.Pod{ID: "pod-existing"}
		monitor.resctrlGroups["pod-existing"] = true

		pods := &resource.Pods{
			Running:    map[string]*resource.Pod{"pod-existing": pod},
			Terminated: map[string]*resource.Pod{},
		}
		resInformer.On("Pods").Return(pods)

		// The pod now has two PIDs — perhaps a new container started since last sync.
		// Child processes of already-tracked PIDs are inherited by the kernel via fork,
		// but new container init processes need explicit re-sync.
		processes := &resource.Processes{
			Running: map[int]*resource.Process{
				10: {PID: 10, Container: &resource.Container{ID: "c-existing"}},
				20: {PID: 20, Container: &resource.Container{ID: "c-existing"}},
			},
			Terminated: map[int]*resource.Process{},
		}
		resInformer.On("Processes").Return(processes)

		containers := &resource.Containers{
			Running: map[string]*resource.Container{
				"c-existing": {ID: "c-existing", Pod: pod},
			},
			Terminated: map[string]*resource.Container{},
		}
		resInformer.On("Containers").Return(containers)

		resctrlMeter.On("AddPIDsToGroup", "pod-existing", mock.MatchedBy(func(pids []int) bool {
			return len(pids) == 2
		})).Return(nil)

		monitor.manageResctrlGroups()

		assert.True(t, monitor.resctrlGroups["pod-existing"],
			"Already-tracked pod should remain tracked")

		resctrlMeter.AssertExpectations(t)
	})

	t.Run("active_mode_reconciles_orphaned_groups_on_startup", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)
		monitor.resctrlPassiveMode = false
		// resctrlReconciled starts false — simulates fresh daemon start.
		assert.False(t, monitor.resctrlReconciled)

		pod := &resource.Pod{ID: "running-pod"}
		pods := &resource.Pods{
			Running:    map[string]*resource.Pod{"running-pod": pod},
			Terminated: map[string]*resource.Pod{},
		}
		resInformer.On("Pods").Return(pods)

		// Simulate orphaned groups left by a crashed previous instance.
		// "running-pod" still exists; "dead-pod" does not.
		discovered := map[string][]int{
			"running-pod": {100},
			"dead-pod":    {200},
		}
		resctrlMeter.On("DiscoverGroups").Return(discovered, nil)
		resctrlMeter.On("DeleteMonitorGroup", "dead-pod").Return(nil)

		// After reconciliation, syncResctrlGroups creates/updates groups
		// for running pods. "running-pod" was adopted so it gets a PID resync.
		processes := &resource.Processes{
			Running: map[int]*resource.Process{
				100: {PID: 100, Container: &resource.Container{ID: "c1"}},
			},
			Terminated: map[int]*resource.Process{},
		}
		resInformer.On("Processes").Return(processes)
		containers := &resource.Containers{
			Running: map[string]*resource.Container{
				"c1": {ID: "c1", Pod: pod},
			},
			Terminated: map[string]*resource.Container{},
		}
		resInformer.On("Containers").Return(containers)
		resctrlMeter.On("AddPIDsToGroup", "running-pod", []int{100}).Return(nil)

		monitor.manageResctrlGroups()

		assert.True(t, monitor.resctrlReconciled,
			"Reconciliation flag should be set after first sync")
		assert.True(t, monitor.resctrlGroups["running-pod"],
			"Running pod's group should be adopted")
		assert.False(t, monitor.resctrlGroups["dead-pod"],
			"Orphaned group should be removed")

		resctrlMeter.AssertExpectations(t)
	})

	t.Run("active_mode_reconciles_only_once", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)
		monitor.resctrlPassiveMode = false
		// Simulate already-reconciled daemon (i.e., second cycle).
		monitor.resctrlReconciled = true

		pods := &resource.Pods{
			Running:    map[string]*resource.Pod{},
			Terminated: map[string]*resource.Pod{},
		}
		resInformer.On("Pods").Return(pods)
		resInformer.On("Processes").Return(&resource.Processes{
			Running:    map[int]*resource.Process{},
			Terminated: map[int]*resource.Process{},
		})
		resInformer.On("Containers").Return(&resource.Containers{
			Running:    map[string]*resource.Container{},
			Terminated: map[string]*resource.Container{},
		})

		monitor.manageResctrlGroups()

		// DiscoverGroups should NOT have been called — reconciliation is skipped.
		resctrlMeter.AssertNotCalled(t, "DiscoverGroups")
	})

	t.Run("graceful_shutdown_cleans_up_groups", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)
		monitor.resctrlPassiveMode = false

		// Simulate two tracked groups at shutdown time.
		monitor.resctrlGroups["pod-a"] = true
		monitor.resctrlGroups["pod-b"] = true

		resctrlMeter.On("DeleteMonitorGroup", "pod-a").Return(nil)
		resctrlMeter.On("DeleteMonitorGroup", "pod-b").Return(nil)

		monitor.cleanupResctrlGroups()

		assert.Empty(t, monitor.resctrlGroups,
			"All groups should be removed after cleanup")
		resctrlMeter.AssertExpectations(t)
	})

	t.Run("graceful_shutdown_tolerates_delete_failures", func(t *testing.T) {
		resctrlMeter := &MockResctrlMeter{}
		resInformer := &MockResourceInformer{}
		monitor := createMonitorWithResctrl(zones, resctrlMeter, resInformer)
		monitor.resctrlPassiveMode = false

		monitor.resctrlGroups["pod-ok"] = true
		monitor.resctrlGroups["pod-fail"] = true

		resctrlMeter.On("DeleteMonitorGroup", "pod-ok").Return(nil)
		resctrlMeter.On("DeleteMonitorGroup", "pod-fail").Return(fmt.Errorf("permission denied"))

		monitor.cleanupResctrlGroups()

		// Map is cleared regardless — we log errors but don't keep stale state
		// since the daemon is shutting down.
		assert.Empty(t, monitor.resctrlGroups)
		resctrlMeter.AssertExpectations(t)
	})
}

func TestCollectAllPodPIDs(t *testing.T) {
	pod := &resource.Pod{ID: "target-pod"}
	otherPod := &resource.Pod{ID: "other-pod"}

	// Processes reference containers by ID only (no .Pod set, matching real informer)
	processes := &resource.Processes{
		Running: map[int]*resource.Process{
			1: {PID: 1, Container: &resource.Container{ID: "c1"}},
			2: {PID: 2, Container: &resource.Container{ID: "c1"}},
			3: {PID: 3, Container: &resource.Container{ID: "c2"}},
			4: {PID: 4}, // no container
		},
		Terminated: map[int]*resource.Process{},
	}

	// Containers have .Pod set (this is the canonical pod linkage)
	containers := &resource.Containers{
		Running: map[string]*resource.Container{
			"c1": {ID: "c1", Pod: pod},
			"c2": {ID: "c2", Pod: otherPod},
		},
		Terminated: map[string]*resource.Container{},
	}

	podPIDs := collectAllPodPIDs(processes, containers)

	assert.Len(t, podPIDs["target-pod"], 2)
	assert.Contains(t, podPIDs["target-pod"], 1)
	assert.Contains(t, podPIDs["target-pod"], 2)

	assert.Len(t, podPIDs["other-pod"], 1)
	assert.Contains(t, podPIDs["other-pod"], 3)
}
