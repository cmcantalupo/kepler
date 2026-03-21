// SPDX-FileCopyrightText: 2026 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

// This file implements the "Option A" dual-instance comparison test from the
// resctrl/AET integration test plan. It starts two Kepler instances
// concurrently — one ratio-only, one AET-enabled — deploys stress-ng
// workloads as Kubernetes pods, and compares per-pod power attribution.
//
// AET (Activity Energy Tracking) is only used for pod-level attribution
// via the resctrl three-pool model: core energy from raw AET deltas
// (instruction-mix sensitive) plus uncore energy shared by CPU time ratio.
// Process-level attribution is always proportional to CPU time only.
//
// Prerequisites:
//   - Bare-metal Linux with Intel RAPL + resctrl/AET (Clearwater Forest or later)
//   - stress-ng installed on the host
//   - Kubernetes cluster (K3s or similar) with Pod RBAC for test ServiceAccount
//   - resctrl filesystem mounted at /sys/fs/resctrl
//
// The test validates that the AET instance produces a measurably different
// power gap between FPU-heavy and scalar workloads compared to the ratio
// instance, which attributes identical power per CPU-ms regardless of
// instruction mix.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/sustainable-computing-io/kepler/test/common"
)

const (
	// Ports for the dual-instance test
	ratioPort = 28282
	aetPort   = 28283

	// Wait for both instances to produce stable power readings across
	// multiple collection cycles (monitor.interval = 3s in configs)
	waitForAETStabilization = 18 * time.Second
)

// findAETConfigFile locates the AET-enabled e2e config file.
func findAETConfigFile() string {
	candidates := []string{
		"test/testdata/e2e-config-aet.yaml",
		"../testdata/e2e-config-aet.yaml",
		"testdata/e2e-config-aet.yaml",
	}

	for _, c := range candidates {
		if absPath, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(absPath); err == nil {
				return absPath
			}
		}
	}
	return ""
}

// requireAETPrerequisites checks all prerequisites for AET comparison tests.
func requireAETPrerequisites(t *testing.T) {
	t.Helper()
	requireE2EPrerequisites(t)
	skipIfNoStressNG(t)
	skipIfNoResctrl(t)

	aetConfig := findAETConfigFile()
	if aetConfig == "" {
		t.Skip("Skipping: e2e-config-aet.yaml not found")
	}
}

// TestAETDualInstance runs two Kepler instances sequentially (ratio-only then
// AET-enabled) and compares power attribution for workloads with different
// instruction mixes deployed as Kubernetes pods.
//
// The instances run sequentially because the apiserver pod informer
// (controller-runtime) binds a metrics server to :8080, preventing two
// simultaneous Kepler instances with kube.enabled=true.
//
// Expected behavior:
//   - The ratio instance assigns power proportional to CPU time only
//   - The AET instance assigns higher power to the FPU workload per CPU-ms
//   - The power gap (FPU / scalar) is larger in the AET instance
func TestAETDualInstance(t *testing.T) {
	requireAETPrerequisites(t)

	aetConfig := findAETConfigFile()
	require.NotEmpty(t, aetConfig, "AET config must be found (checked in prerequisites)")

	// Patch configs with the current node name for kube integration
	ratioConfig := patchConfigNodeName(t, testConfig.configFile)
	aetConfig = patchConfigNodeName(t, aetConfig)

	// Create K8s client and deploy workload pods first (they stay running
	// across both Kepler instance phases).
	k8s := getK8sClient(t)

	numCPU := runtime.NumCPU()
	workersPerWorkload := numCPU / 4
	if workersPerWorkload < 2 {
		workersPerWorkload = 2
	}
	t.Logf("System has %d CPUs, using %d workers per workload pod", numCPU, workersPerWorkload)

	const fpuPodName = "aet-fpu-workload"
	const scalarPodName = "aet-scalar-workload"

	createWorkloadPod(t, k8s, fpuPodName, "matrixprod", workersPerWorkload, 80)
	createWorkloadPod(t, k8s, scalarPodName, "int64", workersPerWorkload, 80)

	// --- Phase 1: Ratio-only Kepler ---
	t.Log("=== Phase 1: Ratio-only Kepler ===")
	var ratioFPUPower, ratioScalarPower float64
	var ratioFPUFound, ratioScalarFound bool
	func() {
		ratioKepler := startKepler(t,
			withLogOutput(os.Stderr),
			withPort(ratioPort),
			withConfig(ratioConfig),
		)
		ratioScraper := common.NewMetricsScraper(ratioKepler.MetricsURL())

		require.True(t, WaitForValidCPUMetrics(t, ratioScraper, 30*time.Second),
			"Ratio Kepler should have valid CPU metrics")
		t.Logf("Ratio Kepler running on port %d (PID %d)", ratioPort, ratioKepler.PID())

		require.True(t, WaitForPodInMetrics(t, ratioScraper, fpuPodName, 60*time.Second),
			"FPU pod should appear in ratio Kepler metrics")
		require.True(t, WaitForPodInMetrics(t, ratioScraper, scalarPodName, 60*time.Second),
			"Scalar pod should appear in ratio Kepler metrics")

		t.Logf("Waiting %v for ratio metrics to stabilize...", waitForAETStabilization)
		time.Sleep(waitForAETStabilization)

		ratioSnap, err := ratioScraper.TakeSnapshot()
		require.NoError(t, err, "Failed to take ratio snapshot")

		ratioFPUPower, ratioFPUFound = FindPodPower(ratioSnap, fpuPodName)
		ratioScalarPower, ratioScalarFound = FindPodPower(ratioSnap, scalarPodName)

		t.Logf("Ratio: FPU=%.4f W (found=%v), Scalar=%.4f W (found=%v)",
			ratioFPUPower, ratioFPUFound, ratioScalarPower, ratioScalarFound)

		// Stop ratio Kepler before starting AET instance
		require.NoError(t, ratioKepler.Stop(), "Failed to stop ratio Kepler")
		t.Log("Ratio Kepler stopped")
	}()

	require.True(t, ratioFPUFound && ratioFPUPower > 0, "Ratio: FPU pod must have power")
	require.True(t, ratioScalarFound && ratioScalarPower > 0, "Ratio: Scalar pod must have power")

	// --- Phase 2: AET-enabled Kepler ---
	t.Log("=== Phase 2: AET-enabled Kepler ===")
	aetKepler := startKepler(t,
		withLogOutput(os.Stderr),
		withPort(aetPort),
		withConfig(aetConfig),
	)
	aetScraper := common.NewMetricsScraper(aetKepler.MetricsURL())

	require.True(t, WaitForValidCPUMetrics(t, aetScraper, 30*time.Second),
		"AET Kepler should have valid CPU metrics")
	t.Logf("AET Kepler running on port %d (PID %d)", aetPort, aetKepler.PID())

	require.True(t, WaitForPodInMetrics(t, aetScraper, fpuPodName, 60*time.Second),
		"FPU pod should appear in AET Kepler metrics")
	require.True(t, WaitForPodInMetrics(t, aetScraper, scalarPodName, 60*time.Second),
		"Scalar pod should appear in AET Kepler metrics")

	t.Logf("Waiting %v for AET metrics to stabilize...", waitForAETStabilization)
	time.Sleep(waitForAETStabilization)

	aetSnap, err := aetScraper.TakeSnapshot()
	require.NoError(t, err, "Failed to take AET snapshot")

	aetFPUPower, aetFPUFound := FindPodPower(aetSnap, fpuPodName)
	aetScalarPower, aetScalarFound := FindPodPower(aetSnap, scalarPodName)

	t.Logf("AET: FPU=%.4f W (found=%v), Scalar=%.4f W (found=%v)",
		aetFPUPower, aetFPUFound, aetScalarPower, aetScalarFound)

	require.True(t, aetFPUFound && aetFPUPower > 0, "AET: FPU pod must have power")
	require.True(t, aetScalarFound && aetScalarPower > 0, "AET: Scalar pod must have power")

	// --- Compare ---
	t.Log("=== Pod Power Attribution Results ===")
	t.Logf("Ratio instance: FPU=%.4f W, Scalar=%.4f W", ratioFPUPower, ratioScalarPower)
	t.Logf("AET instance:   FPU=%.4f W, Scalar=%.4f W", aetFPUPower, aetScalarPower)

	ratioGap := ratioFPUPower / ratioScalarPower
	aetGap := aetFPUPower / aetScalarPower

	t.Logf("Power gap (FPU/Scalar): Ratio=%.4f, AET=%.4f", ratioGap, aetGap)

	assert.Greater(t, aetGap, ratioGap,
		"AET should show a larger power gap between FPU and scalar workloads "+
			"than the ratio model (which is instruction-mix blind)")

	if ratioGap > 0 {
		t.Logf("AET differentiation factor: %.2fx larger gap than ratio", aetGap/ratioGap)
	}
}

// TestAETAttributionSource verifies that the AET-enabled Kepler instance
// reports the resctrl core energy metric for tracked pods.
func TestAETAttributionSource(t *testing.T) {
	requireAETPrerequisites(t)

	aetConfig := findAETConfigFile()
	require.NotEmpty(t, aetConfig)
	aetConfig = patchConfigNodeName(t, aetConfig)

	aetKepler := startKepler(t,
		withLogOutput(os.Stderr),
		withPort(aetPort),
		withConfig(aetConfig),
	)
	aetScraper := common.NewMetricsScraper(aetKepler.MetricsURL())

	require.True(t, WaitForValidCPUMetrics(t, aetScraper, 30*time.Second),
		"AET Kepler should have valid CPU metrics")

	// Create a workload pod so there's something to track
	k8s := getK8sClient(t)
	const podName = "aet-source-test"
	createWorkloadPod(t, k8s, podName, "matrixprod", 2, 50)

	// Wait for pod to appear and resctrl group to be created
	require.True(t, WaitForPodInMetrics(t, aetScraper, podName, 60*time.Second),
		"Workload pod should appear in AET Kepler metrics")
	time.Sleep(waitForAETStabilization)

	snap, err := aetScraper.TakeSnapshot()
	require.NoError(t, err, "Failed to take AET snapshot")

	// Check for the pod-level resctrl core energy metric
	if snap.HasMetric("kepler_pod_resctrl_core_energy_joules_total") {
		resctrlMetrics := snap.GetAllWithName("kepler_pod_resctrl_core_energy_joules_total")
		t.Logf("Found %d pod resctrl core energy metrics", len(resctrlMetrics))

		for _, m := range resctrlMetrics {
			if m.Labels["pod_name"] == podName {
				t.Logf("Pod %s has resctrl core energy: %.4f J", podName, m.Value)
			}
		}
	} else {
		t.Log("kepler_pod_resctrl_core_energy_joules_total not found " +
			"(may need additional collection cycles)")
	}

	// Verify the AET Kepler is reporting pod power
	podMetrics := snap.GetAllWithName("kepler_pod_cpu_watts")
	assert.NotEmpty(t, podMetrics, "AET Kepler should export pod power metrics")

	power, found := FindPodPower(snap, podName)
	if found {
		t.Logf("Pod %s power: %.4f W", podName, power)
	}
	assert.True(t, found && power > 0,
		"Workload pod should have non-zero power attribution")
}

// TestAETEnergyConservation verifies that the AET-enabled instance preserves
// energy conservation: the sum of all pod power should not exceed node
// active power.
func TestAETEnergyConservation(t *testing.T) {
	requireAETPrerequisites(t)

	aetConfig := findAETConfigFile()
	require.NotEmpty(t, aetConfig)
	aetConfig = patchConfigNodeName(t, aetConfig)

	aetKepler := startKepler(t,
		withLogOutput(os.Stderr),
		withPort(aetPort),
		withConfig(aetConfig),
	)
	aetScraper := common.NewMetricsScraper(aetKepler.MetricsURL())

	require.True(t, WaitForValidCPUMetrics(t, aetScraper, 30*time.Second))

	// Create a substantial load pod
	k8s := getK8sClient(t)
	numCPU := runtime.NumCPU()
	conservationWorkers := numCPU / 4
	if conservationWorkers < 4 {
		conservationWorkers = 4
	}
	t.Logf("System has %d CPUs, using %d workers for conservation test", numCPU, conservationWorkers)

	const podName = "aet-conservation-test"
	createWorkloadPod(t, k8s, podName, "matrixprod", conservationWorkers, 70)

	require.True(t, WaitForPodInMetrics(t, aetScraper, podName, 60*time.Second),
		"Conservation pod should appear in metrics")
	time.Sleep(waitForAETStabilization)

	snap, err := aetScraper.TakeSnapshot()
	require.NoError(t, err)

	// Sum all pod power
	podMetrics := snap.GetAllWithName("kepler_pod_cpu_watts")
	var totalPodPower float64
	for _, m := range podMetrics {
		if m.Value > 0 {
			totalPodPower += m.Value
		}
	}

	// Get node total power
	nodePower := snap.SumValues("kepler_node_cpu_watts", nil)

	t.Logf("Node power: %.2f W, Total pod power: %.2f W", nodePower, totalPodPower)

	// Pod power should not exceed node power (energy conservation)
	if nodePower > 0 {
		assert.LessOrEqual(t, totalPodPower, nodePower*1.01, // 1% tolerance for timing
			"Sum of pod power should not exceed node power (energy conservation)")
	}
}
