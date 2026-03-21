// SPDX-FileCopyrightText: 2025 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/sustainable-computing-io/kepler/test/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// RAPLPath is the path to the RAPL sysfs interface
	RAPLPath = "/sys/class/powercap/intel-rapl"

	// StressNGBinary is the name of the stress-ng binary
	StressNGBinary = "stress-ng"
)

// skipIfNoRAPL skips the test if RAPL is not available
func skipIfNoRAPL(t *testing.T) {
	t.Helper()

	if _, err := os.Stat(RAPLPath); os.IsNotExist(err) {
		t.Skipf("Skipping: RAPL not available at %s", RAPLPath)
	}

	entries, err := os.ReadDir(RAPLPath)
	if err != nil {
		t.Skipf("Skipping: Cannot read RAPL directory: %v", err)
	}

	if len(entries) == 0 {
		t.Skipf("Skipping: No RAPL zones found at %s", RAPLPath)
	}
}

// skipIfNoStressNG skips the test if stress-ng is not available
func skipIfNoStressNG(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath(StressNGBinary); err != nil {
		t.Skipf("Skipping: stress-ng not found (install: apt-get install stress-ng)")
	}
}

// skipIfRAPLNotReadable skips the test if RAPL energy values are not readable
func skipIfRAPLNotReadable(t *testing.T) {
	t.Helper()

	energyPath := filepath.Join(RAPLPath, "intel-rapl:0", "energy_uj")
	if _, err := os.ReadFile(energyPath); err != nil {
		t.Skipf("Skipping: RAPL energy not readable at %s: %v", energyPath, err)
	}
}

// ResctrlPath is the path to the resctrl filesystem
const ResctrlPath = "/sys/fs/resctrl"

// skipIfNoResctrl skips the test if the resctrl filesystem is not available
// or does not support AET core_energy counters.
func skipIfNoResctrl(t *testing.T) {
	t.Helper()

	if _, err := os.Stat(ResctrlPath); os.IsNotExist(err) {
		t.Skipf("Skipping: resctrl not available at %s", ResctrlPath)
	}

	// Check for AET core_energy support by looking for mon_data directories
	monDataPath := filepath.Join(ResctrlPath, "mon_data")
	entries, err := os.ReadDir(monDataPath)
	if err != nil {
		t.Skipf("Skipping: cannot read resctrl mon_data: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "mon_PERF_PKG_") {
			coreEnergyPath := filepath.Join(monDataPath, e.Name(), "core_energy")
			if _, err := os.Stat(coreEnergyPath); err == nil {
				return // AET core_energy is available
			}
		}
	}

	t.Skipf("Skipping: resctrl mon_data has no AET core_energy counters")
}

// requireE2EPrerequisites checks all common prerequisites for e2e tests
func requireE2EPrerequisites(t *testing.T) {
	t.Helper()
	skipIfNoRAPL(t)
	skipIfRAPLNotReadable(t)
}

// requireWorkloadPrerequisites checks prerequisites for workload tests
func requireWorkloadPrerequisites(t *testing.T) {
	t.Helper()
	requireE2EPrerequisites(t)
	skipIfNoStressNG(t)
}

// KeplerInstance manages a Kepler process for testing
type KeplerInstance struct {
	cmd        *exec.Cmd
	configPath string
	binaryPath string
	port       int
	logOutput  io.Writer
}

// keplerOption configures KeplerInstance
type keplerOption func(*KeplerInstance)

// withLogOutput sets where to write logs
func withLogOutput(w io.Writer) keplerOption {
	return func(k *KeplerInstance) { k.logOutput = w }
}

// withPort overrides the metrics port
func withPort(port int) keplerOption {
	return func(k *KeplerInstance) { k.port = port }
}

// withConfig overrides the config file path
func withConfig(path string) keplerOption {
	return func(k *KeplerInstance) { k.configPath = path }
}

// startKepler starts Kepler and registers cleanup
func startKepler(t *testing.T, opts ...keplerOption) *KeplerInstance {
	t.Helper()

	k := &KeplerInstance{
		port:       testConfig.metricsPort,
		binaryPath: testConfig.keplerBinary,
		configPath: testConfig.configFile,
		logOutput:  io.Discard,
	}

	for _, opt := range opts {
		opt(k)
	}

	if err := k.start(t); err != nil {
		t.Fatalf("Failed to start Kepler: %v", err)
	}

	t.Cleanup(func() {
		if err := k.stop(); err != nil {
			t.Logf("Warning: Failed to stop Kepler: %v", err)
		}
	})

	return k
}

func (k *KeplerInstance) start(t *testing.T) error {
	t.Helper()

	binaryPath, err := k.findBinary()
	if err != nil {
		return err
	}

	args := []string{
		fmt.Sprintf("--web.listen-address=:%d", k.port),
		"--log.level=debug",
	}
	if k.configPath != "" {
		args = append(args, fmt.Sprintf("--config.file=%s", k.configPath))
	}

	k.cmd = exec.Command(binaryPath, args...)

	stdout, _ := k.cmd.StdoutPipe()
	stderr, _ := k.cmd.StderrPipe()

	// Forward logs in background (errors are not actionable here)
	go func() { _, _ = io.Copy(k.logOutput, stdout) }()
	go func() { _, _ = io.Copy(k.logOutput, stderr) }()

	t.Logf("Starting Kepler: %s %v", binaryPath, args)

	if err := k.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start kepler: %w", err)
	}

	if err := k.waitForReady(30 * time.Second); err != nil {
		_ = k.stop() // Best-effort cleanup
		return fmt.Errorf("kepler failed to become ready: %w", err)
	}

	t.Logf("Kepler started on port %d (PID: %d)", k.port, k.cmd.Process.Pid)
	return nil
}

func (k *KeplerInstance) findBinary() (string, error) {
	if _, err := os.Stat(k.binaryPath); err == nil {
		return filepath.Abs(k.binaryPath)
	}
	if path, err := exec.LookPath("kepler"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("kepler binary not found at %s or in PATH", k.binaryPath)
}

func (k *KeplerInstance) waitForReady(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	url := fmt.Sprintf("http://localhost:%d/metrics", k.port)

	return common.WaitForCondition(ctx, 500*time.Millisecond, func() bool {
		scraper := common.NewMetricsScraper(url)
		_, err := scraper.Scrape()
		return err == nil
	})
}

func (k *KeplerInstance) stop() error {
	if k.cmd == nil || k.cmd.Process == nil {
		return nil
	}

	// Send SIGTERM for graceful shutdown
	_ = k.cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() {
		_, err := k.cmd.Process.Wait()
		done <- err
	}()

	select {
	case <-time.After(10 * time.Second):
		_ = k.cmd.Process.Kill() // Force kill on timeout
		return fmt.Errorf("kepler did not stop gracefully")
	case <-done:
		return nil
	}
}

// MetricsURL returns the metrics endpoint URL
func (k *KeplerInstance) MetricsURL() string {
	return fmt.Sprintf("http://localhost:%d/metrics", k.port)
}

// BaseURL returns the base URL for the Kepler server
func (k *KeplerInstance) BaseURL() string {
	return fmt.Sprintf("http://localhost:%d", k.port)
}

// PID returns the Kepler process PID
func (k *KeplerInstance) PID() int {
	if k.cmd == nil || k.cmd.Process == nil {
		return 0
	}
	return k.cmd.Process.Pid
}

// IsRunning returns true if Kepler is running
func (k *KeplerInstance) IsRunning() bool {
	if k.cmd == nil || k.cmd.Process == nil {
		return false
	}
	return k.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// Stop stops Kepler
func (k *KeplerInstance) Stop() error {
	return k.stop()
}

// Workload represents a stress-ng workload
type Workload struct {
	cmd        *exec.Cmd
	workerPIDs []int
	name       string
}

// workloadOption configures a workload
type workloadOption func(*workloadConfig)

type workloadConfig struct {
	name       string
	cpuWorkers int
	cpuLoad    int
	cpuMethod  string
}

// WithWorkloadName sets workload name
func WithWorkloadName(name string) workloadOption {
	return func(c *workloadConfig) { c.name = name }
}

// WithCPUWorkers sets CPU worker count
func WithCPUWorkers(n int) workloadOption {
	return func(c *workloadConfig) { c.cpuWorkers = n }
}

// WithCPULoad sets CPU load percentage
func WithCPULoad(percent int) workloadOption {
	return func(c *workloadConfig) { c.cpuLoad = percent }
}

// WithCPUMethod sets the stress-ng CPU stressor method (e.g., "matrixprod", "int64")
func WithCPUMethod(method string) workloadOption {
	return func(c *workloadConfig) { c.cpuMethod = method }
}

// StartWorkload starts a stress workload
func StartWorkload(t *testing.T, opts ...workloadOption) *Workload {
	t.Helper()

	cfg := workloadConfig{name: "stress", cpuWorkers: 1, cpuLoad: 50}
	for _, opt := range opts {
		opt(&cfg)
	}

	args := []string{"--cpu", strconv.Itoa(cfg.cpuWorkers)}
	if cfg.cpuLoad > 0 && cfg.cpuLoad <= 100 {
		args = append(args, "--cpu-load", strconv.Itoa(cfg.cpuLoad))
	}
	if cfg.cpuMethod != "" {
		args = append(args, "--cpu-method", cfg.cpuMethod)
	}
	args = append(args, "--metrics-brief")

	t.Logf("Starting workload %q: stress-ng %s", cfg.name, strings.Join(args, " "))

	cmd := exec.Command(StressNGBinary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start stress-ng: %v", err)
	}

	w := &Workload{cmd: cmd, name: cfg.name}

	// Retry child PID discovery — stress-ng may take time to fork workers
	for attempt := 0; attempt < 10; attempt++ {
		time.Sleep(500 * time.Millisecond)
		w.workerPIDs = findChildPIDs(cmd.Process.Pid)
		if len(w.workerPIDs) >= cfg.cpuWorkers {
			break
		}
	}

	t.Logf("Workload %q started: PID=%d, workers=%v", cfg.name, cmd.Process.Pid, w.workerPIDs)

	t.Cleanup(func() {
		_ = w.Stop() // Best-effort cleanup
	})

	return w
}

func findChildPIDs(parentPID int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	var children []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}

		// The comm field (field 2) is enclosed in parentheses and may
		// contain spaces (e.g. "(stress-ng-cpu)"). Find the closing
		// paren to reliably locate subsequent fields.
		statStr := string(data)
		closeParen := strings.LastIndex(statStr, ")")
		if closeParen < 0 {
			continue
		}
		fields := strings.Fields(statStr[closeParen+1:])
		// fields[0] = state, fields[1] = ppid
		if len(fields) >= 2 {
			if ppid, _ := strconv.Atoi(fields[1]); ppid == parentPID {
				children = append(children, pid)
			}
		}
	}
	return children
}

// Stop stops the workload
func (w *Workload) Stop() error {
	if w.cmd == nil || w.cmd.Process == nil {
		return nil
	}

	pgid, err := syscall.Getpgid(w.cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM) // Signal entire process group
	}

	done := make(chan error, 1)
	go func() {
		_, err := w.cmd.Process.Wait()
		done <- err
	}()

	select {
	case <-time.After(5 * time.Second):
		// Force kill on timeout
		if pgid != 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		_ = w.cmd.Process.Kill()
		return fmt.Errorf("workload killed")
	case <-done:
		return nil
	}
}

// PIDs returns worker PIDs
func (w *Workload) PIDs() []int { return w.workerPIDs }

// ParentPID returns parent PID
func (w *Workload) ParentPID() int {
	if w.cmd == nil || w.cmd.Process == nil {
		return 0
	}
	return w.cmd.Process.Pid
}

// WaitForValidCPUMetrics waits for Kepler to have valid CPU metrics.
func WaitForValidCPUMetrics(t *testing.T, scraper *common.MetricsScraper, timeout time.Duration) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var usageRatio float64
	var powerValue float64

	err := common.WaitForCondition(ctx, 2*time.Second, func() bool {
		usageMetrics, err := scraper.ScrapeMetric("kepler_node_cpu_usage_ratio")
		if err != nil || len(usageMetrics) == 0 || usageMetrics[0].Value <= 0 {
			return false
		}
		usageRatio = usageMetrics[0].Value

		powerMetrics, err := scraper.ScrapeMetric("kepler_node_cpu_watts")
		if err != nil || len(powerMetrics) == 0 {
			return false
		}
		for _, m := range powerMetrics {
			if m.Value > 0 {
				powerValue = m.Value
				return true
			}
		}
		return false
	})
	if err != nil {
		t.Log("Timeout waiting for valid CPU metrics")
		return false
	}

	t.Logf("CPU metrics ready: usage_ratio=%.4f, power=%.2f W", usageRatio, powerValue)
	return true
}

// WaitForProcessInMetrics waits for a process to appear in metrics
func WaitForProcessInMetrics(t *testing.T, scraper *common.MetricsScraper, pid int, timeout time.Duration) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pidStr := strconv.Itoa(pid)

	err := common.WaitForCondition(ctx, 1*time.Second, func() bool {
		metrics, err := scraper.ScrapeMetric("kepler_process_cpu_watts")
		if err != nil {
			return false
		}
		for _, m := range metrics {
			if m.Labels["pid"] == pidStr {
				return true
			}
		}
		return false
	})
	if err != nil {
		return false
	}

	t.Logf("Found PID=%d in metrics", pid)
	return true
}

// WaitForNonZeroProcessPower waits for a process to show non-zero power
func WaitForNonZeroProcessPower(t *testing.T, scraper *common.MetricsScraper, pid int, timeout time.Duration) (float64, bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pidStr := strconv.Itoa(pid)
	var foundPower float64

	err := common.WaitForCondition(ctx, 1*time.Second, func() bool {
		metrics, err := scraper.ScrapeMetric("kepler_process_cpu_watts")
		if err != nil {
			return false
		}
		for _, m := range metrics {
			if m.Labels["pid"] == pidStr && m.Value > 0 {
				foundPower = m.Value
				return true
			}
		}
		return false
	})
	if err != nil {
		return 0, false
	}

	return foundPower, true
}

// FindWorkloadPower searches for a workload's power in a metrics snapshot.
func FindWorkloadPower(snapshot *common.MetricsSnapshot, w *Workload) (power float64, found bool) {
	processMetrics := snapshot.GetAllWithName("kepler_process_cpu_watts")

	parentPIDStr := strconv.Itoa(w.ParentPID())
	for _, m := range processMetrics {
		if m.Labels["pid"] == parentPIDStr && m.Labels["state"] == "running" && m.Value > 0 {
			return m.Value, true
		}
	}

	for _, pid := range w.PIDs() {
		pidStr := strconv.Itoa(pid)
		for _, m := range processMetrics {
			if m.Labels["pid"] == pidStr && m.Labels["state"] == "running" {
				power += m.Value
				found = true
			}
		}
	}
	return power, found
}

// WaitForTerminatedProcess waits for a process to appear in metrics with state=terminated
func WaitForTerminatedProcess(t *testing.T, scraper *common.MetricsScraper, pid int, timeout time.Duration) (energy float64, found bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pidStr := strconv.Itoa(pid)

	err := common.WaitForCondition(ctx, 2*time.Second, func() bool {
		metrics, err := scraper.ScrapeMetric("kepler_process_cpu_joules_total")
		if err != nil {
			return false
		}
		for _, m := range metrics {
			if m.Labels["pid"] == pidStr && m.Labels["state"] == "terminated" {
				energy = m.Value
				found = true
				return true
			}
		}
		return false
	})
	if err != nil {
		return 0, false
	}

	t.Logf("Found terminated PID=%d with energy=%.4f J", pid, energy)
	return energy, true
}

// --- Kubernetes pod helpers for AET tests ---

const workloadNamespace = "kepler"

// getNodeName returns the Kubernetes node name from the KUBERNETES_NODE_NAME
// env var (set via downward API) or falls back to os.Hostname().
func getNodeName(t *testing.T) string {
	t.Helper()
	if n := os.Getenv("KUBERNETES_NODE_NAME"); n != "" {
		return n
	}
	h, err := os.Hostname()
	require.NoError(t, err, "Failed to get hostname")
	return h
}

// patchConfigNodeName reads a Kepler config file, sets kube.nodeName to the
// current node, and writes the result to a temp file. Returns the temp path.
func patchConfigNodeName(t *testing.T, configPath string) string {
	t.Helper()

	data, err := os.ReadFile(configPath)
	require.NoError(t, err, "Failed to read config %s", configPath)

	nodeName := getNodeName(t)
	content := strings.ReplaceAll(string(data),
		`nodeName: ""`, fmt.Sprintf("nodeName: %q", nodeName))

	tmpFile := filepath.Join(t.TempDir(), filepath.Base(configPath))
	err = os.WriteFile(tmpFile, []byte(content), 0o600)
	require.NoError(t, err)

	t.Logf("Patched config %s → %s (nodeName=%s)", configPath, tmpFile, nodeName)
	return tmpFile
}

// getK8sClient returns a Kubernetes client using in-cluster config.
func getK8sClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	cfg, err := rest.InClusterConfig()
	if err != nil {
		t.Skipf("Cannot create in-cluster config: %v (test must run inside K8s)", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	return client
}

// hostPathDir returns a pointer to HostPathDirectory.
func hostPathDir() *corev1.HostPathType {
	t := corev1.HostPathDirectory
	return &t
}

// createWorkloadPod creates a stress-ng pod on the current node and waits for
// it to reach Running state. Cleanup is registered via t.Cleanup.
func createWorkloadPod(t *testing.T, client kubernetes.Interface, name, method string, workers, cpuLoad int) {
	t.Helper()

	nodeName := getNodeName(t)
	cmd := fmt.Sprintf(
		"/host-lib64/ld-linux-x86-64.so.2 --library-path /host-lib64 "+
			"/host-usr-bin/stress-ng --cpu %d --cpu-load %d --cpu-method %s --metrics-brief -t 0",
		workers, cpuLoad, method)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: workloadNamespace,
			Labels: map[string]string{
				"app":      "kepler-e2e-workload",
				"workload": name,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "stress",
				Image:           "quay.io/sustainable_computing_io/kepler:resctrl-test",
				ImagePullPolicy: corev1.PullNever,
				Command:         []string{"/bin/sh", "-c", cmd},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "host-usr-bin", MountPath: "/host-usr-bin", ReadOnly: true},
					{Name: "host-lib64", MountPath: "/host-lib64", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "host-usr-bin", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/usr/bin", Type: hostPathDir()},
				}},
				{Name: "host-lib64", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/lib64", Type: hostPathDir()},
				}},
			},
		},
	}

	_, err := client.CoreV1().Pods(workloadNamespace).Create(
		context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create workload pod %s", name)

	t.Cleanup(func() {
		_ = client.CoreV1().Pods(workloadNamespace).Delete(
			context.Background(), name, metav1.DeleteOptions{})
		t.Logf("Deleted workload pod %s", name)
	})

	waitForPodRunning(t, client, name, 60*time.Second)
	t.Logf("Workload pod %s running on node %s", name, nodeName)
}

// waitForPodRunning waits until a pod reaches Running phase.
func waitForPodRunning(t *testing.T, client kubernetes.Interface, name string, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := common.WaitForCondition(ctx, 1*time.Second, func() bool {
		pod, err := client.CoreV1().Pods(workloadNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return pod.Status.Phase == corev1.PodRunning
	})
	require.NoError(t, err, "Pod %s did not reach Running within %v", name, timeout)
}

// WaitForPodInMetrics waits for a pod to appear in kepler_pod_cpu_watts.
func WaitForPodInMetrics(t *testing.T, scraper *common.MetricsScraper, podName string, timeout time.Duration) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := common.WaitForCondition(ctx, 2*time.Second, func() bool {
		metrics, err := scraper.ScrapeMetric("kepler_pod_cpu_watts")
		if err != nil {
			return false
		}
		for _, m := range metrics {
			if m.Labels["pod_name"] == podName {
				return true
			}
		}
		return false
	})
	if err != nil {
		return false
	}

	t.Logf("Found pod %s in metrics", podName)
	return true
}

// FindPodPower returns the total power for a pod from a metrics snapshot.
func FindPodPower(snapshot *common.MetricsSnapshot, podName string) (power float64, found bool) {
	for _, m := range snapshot.GetAllWithName("kepler_pod_cpu_watts") {
		if m.Labels["pod_name"] == podName && m.Value > 0 {
			power += m.Value
			found = true
		}
	}
	return power, found
}
