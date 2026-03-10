// SPDX-FileCopyrightText: 2026 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ResctrlPowerMeter reads per-workload core energy from the Linux resctrl
// filesystem exposed by Intel's AET (Application Energy Telemetry) feature.
//
// It supports two modes:
//   - Active mode: creates and manages mon_groups under /sys/fs/resctrl,
//     assigning PIDs to monitoring groups for per-pod energy tracking.
//   - Passive mode: discovers existing UUID-named mon_groups created by
//     an external orchestrator (e.g., a platform-level resource manager).
//
// Only core energy (from mon_PERF_PKG_*/core_energy) is read.
// MBM bandwidth metrics are intentionally omitted.
type ResctrlPowerMeter interface {
	powerMeter

	// Init validates that AET core_energy is available on this system.
	// Returns an error if the resctrl filesystem is not mounted or AET
	// zones are not found.
	Init() error

	// Zones returns the discovered AET zone names (e.g., ["mon_PERF_PKG_0", "mon_PERF_PKG_1"]).
	Zones() []string

	// DiscoverGroups scans for existing mon_groups whose names look like
	// Kubernetes pod UUIDs. Returns a map of group ID → list of PIDs.
	// Used in passive mode.
	DiscoverGroups() (map[string][]int, error)

	// CreateMonitorGroup creates a new mon_group with the given ID and
	// assigns the specified PIDs to it.
	CreateMonitorGroup(id string, pids []int) error

	// DeleteMonitorGroup removes the mon_group with the given ID.
	DeleteMonitorGroup(id string) error

	// AddPIDsToGroup adds PIDs to an existing mon_group.
	AddPIDsToGroup(id string, pids []int) error

	// ReadGroupEnergy reads the cumulative core energy (in Joules, float64)
	// for the specified mon_group, summed across all AET zones.
	// The kernel AET interface reports energy as floating-point Joules;
	// we preserve that precision throughout.
	ReadGroupEnergy(id string) (float64, error)

	// ReadGroupEnergyByZone reads the cumulative core energy (in Joules, float64)
	// for each AET zone individually. Returns a map from zone name
	// (e.g., "mon_PERF_PKG_00") to that zone's cumulative energy.
	// This avoids double-counting on multi-socket systems where
	// each mon_PERF_PKG corresponds to a different CPU package.
	ReadGroupEnergyByZone(id string) (map[string]float64, error)

	// ReadGroupActivityByZone reads the cumulative CPU activity (in CPU-seconds,
	// float64) for each AET zone individually. Returns a map from zone name
	// (e.g., "mon_PERF_PKG_00") to that zone's cumulative activity counter.
	// The activity file is exposed by resctrl alongside core_energy when AET is
	// available. Activity counters measure actual CPU-seconds consumed by the
	// group's processes, unaffected by HT or C-state distortions.
	// Returns an error if the activity files are not available (older kernels).
	ReadGroupActivityByZone(id string) (map[string]float64, error)

	// GroupExists returns true if a mon_group with the given ID exists.
	GroupExists(id string) bool
}

// resctrlFSReader abstracts filesystem operations for testability.
type resctrlFSReader interface {
	// ReadFile reads the contents of a file.
	ReadFile(path string) ([]byte, error)

	// ReadDir lists a directory.
	ReadDir(path string) ([]os.DirEntry, error)

	// MkdirAll creates a directory and all parents.
	MkdirAll(path string, perm os.FileMode) error

	// RemoveAll removes a directory and all children.
	RemoveAll(path string) error

	// WriteFile writes data to a file.
	WriteFile(path string, data []byte, perm os.FileMode) error

	// Stat returns info about a path.
	Stat(path string) (os.FileInfo, error)
}

// osResctrlFSReader implements resctrlFSReader using the real OS filesystem.
type osResctrlFSReader struct{}

func (o *osResctrlFSReader) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (o *osResctrlFSReader) ReadDir(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (o *osResctrlFSReader) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (o *osResctrlFSReader) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (o *osResctrlFSReader) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func (o *osResctrlFSReader) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// uuidPattern matches Kubernetes pod UUIDs like "12345678-1234-1234-1234-123456789abc".
// Kubernetes UIDs are always lowercase hex, so case-insensitive matching is unnecessary.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// resctrlPowerMeterImpl is the concrete implementation of ResctrlPowerMeter.
type resctrlPowerMeterImpl struct {
	mu       sync.RWMutex
	basePath string // e.g., /sys/fs/resctrl
	fs       resctrlFSReader
	logger   *slog.Logger
	zones    []string // discovered AET zone names (e.g., mon_PERF_PKG_0)
}

// ResctrlOption configures a resctrlPowerMeterImpl.
type ResctrlOption func(*resctrlPowerMeterImpl)

// WithResctrlBasePath sets the resctrl filesystem base path.
func WithResctrlBasePath(path string) ResctrlOption {
	return func(r *resctrlPowerMeterImpl) {
		r.basePath = path
	}
}

// WithResctrlLogger sets the logger.
func WithResctrlLogger(logger *slog.Logger) ResctrlOption {
	return func(r *resctrlPowerMeterImpl) {
		r.logger = logger
	}
}

// WithResctrlFSReader sets a custom filesystem reader (for testing).
func WithResctrlFSReader(fs resctrlFSReader) ResctrlOption {
	return func(r *resctrlPowerMeterImpl) {
		r.fs = fs
	}
}

// NewResctrlPowerMeter creates a new ResctrlPowerMeter instance.
func NewResctrlPowerMeter(opts ...ResctrlOption) ResctrlPowerMeter {
	r := &resctrlPowerMeterImpl{
		basePath: "/sys/fs/resctrl",
		fs:       &osResctrlFSReader{},
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *resctrlPowerMeterImpl) Name() string {
	return "resctrl"
}

// Init validates AET support by checking for core_energy files under
// the root mon_data directory.
func (r *resctrlPowerMeterImpl) Init() error {
	monDataPath := filepath.Join(r.basePath, "mon_data")

	entries, err := r.fs.ReadDir(monDataPath)
	if err != nil {
		return fmt.Errorf("resctrl mon_data not accessible at %s: %w", monDataPath, err)
	}

	var zones []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "mon_PERF_PKG_") {
			continue
		}

		// Check that core_energy file exists in this zone
		coreEnergyPath := filepath.Join(monDataPath, name, "core_energy")
		if _, err := r.fs.Stat(coreEnergyPath); err != nil {
			r.logger.Debug("Skipping AET zone without core_energy",
				"zone", name, "error", err)
			continue
		}

		zones = append(zones, name)
	}

	if len(zones) == 0 {
		return fmt.Errorf("no AET core_energy zones found under %s; "+
			"ensure Intel AET is supported and resctrl is mounted", monDataPath)
	}

	sort.Strings(zones)
	r.mu.Lock()
	r.zones = zones
	r.mu.Unlock()

	r.logger.Info("ResctrlPowerMeter initialized",
		"basePath", r.basePath,
		"zones", zones)

	return nil
}

// Zones returns the discovered AET zone names.
func (r *resctrlPowerMeterImpl) Zones() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]string, len(r.zones))
	copy(result, r.zones)
	return result
}

// DiscoverGroups scans for existing UUID-named mon_groups (passive mode).
func (r *resctrlPowerMeterImpl) DiscoverGroups() (map[string][]int, error) {
	monGroupsPath := filepath.Join(r.basePath, "mon_groups")

	entries, err := r.fs.ReadDir(monGroupsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read mon_groups at %s: %w", monGroupsPath, err)
	}

	groups := make(map[string][]int)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !uuidPattern.MatchString(name) {
			continue
		}

		// Read PIDs from the tasks file
		pids, err := r.readTasksPIDs(name)
		if err != nil {
			// In passive mode we only need the group directory name to read
			// core_energy counters, so keep the group with an empty PID list
			// instead of skipping it entirely.
			r.logger.Warn("Failed to read tasks for discovered group; keeping with empty PID list",
				"group", name, "error", err)
			pids = []int{}
		}

		groups[name] = pids
		r.logger.Debug("Discovered resctrl group",
			"group", name, "pids", len(pids))
	}

	return groups, nil
}

// validateGroupID ensures the monitor group ID is safe for use as a path
// component under the resctrl filesystem. It rejects IDs containing path
// separators, "..", or other characters that could escape the base path.
func validateGroupID(id string) error {
	if id == "" {
		return fmt.Errorf("empty monitor group ID")
	}
	if strings.ContainsAny(id, "/\\"+(string)(os.PathSeparator)) || id != filepath.Base(id) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid monitor group ID %q: must be a simple name without path separators", id)
	}
	return nil
}

// CreateMonitorGroup creates a new mon_group and assigns PIDs.
//
// Note: There is a race window between scanning PIDs (by the caller) and
// writing them to the tasks file below. If a tracked process forks during
// that window, the child inherits the default RMID rather than the group's
// RMID. The caller (syncResctrlGroups) mitigates this by re-syncing PIDs
// via AddPIDsToGroup on every subsequent collection cycle, so missed PIDs
// are captured within one cycle. See the "PID assignment race" comment in
// resctrl.go for details.
func (r *resctrlPowerMeterImpl) CreateMonitorGroup(id string, pids []int) error {
	if err := validateGroupID(id); err != nil {
		return err
	}
	groupPath := filepath.Join(r.basePath, "mon_groups", id)

	if err := r.fs.MkdirAll(groupPath, 0o700); err != nil {
		return fmt.Errorf("failed to create mon_group %s: %w", id, err)
	}

	if err := r.writePIDs(id, pids); err != nil {
		// Clean up on failure
		_ = r.fs.RemoveAll(groupPath)
		return fmt.Errorf("failed to assign PIDs to mon_group %s: %w", id, err)
	}

	r.logger.Info("Created resctrl monitoring group",
		"group", id, "pids", len(pids))

	return nil
}

// DeleteMonitorGroup removes the mon_group.
func (r *resctrlPowerMeterImpl) DeleteMonitorGroup(id string) error {
	if err := validateGroupID(id); err != nil {
		return err
	}
	groupPath := filepath.Join(r.basePath, "mon_groups", id)

	if err := r.fs.RemoveAll(groupPath); err != nil {
		return fmt.Errorf("failed to delete mon_group %s: %w", id, err)
	}

	r.logger.Info("Deleted resctrl monitoring group", "group", id)
	return nil
}

// AddPIDsToGroup writes additional PIDs to a group's tasks file.
func (r *resctrlPowerMeterImpl) AddPIDsToGroup(id string, pids []int) error {
	if err := validateGroupID(id); err != nil {
		return err
	}
	return r.writePIDs(id, pids)
}

// ReadGroupEnergy reads the cumulative core energy for a group, summed
// across all AET zones. Returns energy in Joules (float64).
func (r *resctrlPowerMeterImpl) ReadGroupEnergy(id string) (float64, error) {
	if id != "" {
		if err := validateGroupID(id); err != nil {
			return 0, err
		}
	}
	r.mu.RLock()
	zones := r.zones
	r.mu.RUnlock()

	if len(zones) == 0 {
		return 0, fmt.Errorf("no AET zones discovered; call Init() first")
	}

	var totalEnergy float64
	for _, zone := range zones {
		energy, err := r.readZoneFile(id, zone, "core_energy")
		if err != nil {
			return 0, fmt.Errorf("failed to read core_energy for group %s zone %s: %w",
				id, zone, err)
		}
		totalEnergy += energy
	}

	return totalEnergy, nil
}

// ReadGroupEnergyByZone reads the cumulative core energy for a group,
// returning a map from AET zone name to energy in Joules (float64).
// Each zone corresponds to one CPU package (e.g., "mon_PERF_PKG_00" → package 0).
func (r *resctrlPowerMeterImpl) ReadGroupEnergyByZone(id string) (map[string]float64, error) {
	return r.readZoneFileByZone(id, "core_energy")
}

// ReadGroupActivityByZone reads the cumulative CPU activity for a group,
// returning a map from AET zone name to activity in CPU-seconds (float64).
// Each zone corresponds to one CPU package (e.g., "mon_PERF_PKG_00" → package 0).
// Returns an error if activity files are not available (older kernels or
// configurations without AET activity support).
func (r *resctrlPowerMeterImpl) ReadGroupActivityByZone(id string) (map[string]float64, error) {
	return r.readZoneFileByZone(id, "activity")
}

// readZoneFileByZone reads a specific metric file from all AET zones for a group,
// returning a map from zone name to the parsed float64 value.
func (r *resctrlPowerMeterImpl) readZoneFileByZone(id, filename string) (map[string]float64, error) {
	if id != "" {
		if err := validateGroupID(id); err != nil {
			return nil, err
		}
	}
	r.mu.RLock()
	zones := r.zones
	r.mu.RUnlock()

	if len(zones) == 0 {
		return nil, fmt.Errorf("no AET zones discovered; call Init() first")
	}

	result := make(map[string]float64, len(zones))
	for _, zone := range zones {
		val, err := r.readZoneFile(id, zone, filename)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s for group %s zone %s: %w",
				filename, id, zone, err)
		}
		result[zone] = val
	}

	return result, nil
}

// GroupExists checks if a mon_group directory exists.
func (r *resctrlPowerMeterImpl) GroupExists(id string) bool {
	if err := validateGroupID(id); err != nil {
		return false
	}
	groupPath := filepath.Join(r.basePath, "mon_groups", id)
	_, err := r.fs.Stat(groupPath)
	return err == nil
}

// readZoneFile reads a float64 value from a specific metric file in an AET zone
// for a group. Handles both root-level reads (groupID="" reads from mon_data)
// and per-group reads (from mon_groups/<id>/mon_data).
// The kernel AET interface reports counters as floating-point values
// (e.g., "94499439.510380" for core_energy, "1234.567890" for activity).
func (r *resctrlPowerMeterImpl) readZoneFile(groupID, zone, filename string) (float64, error) {
	var filePath string
	if groupID == "" {
		// Root level (no group)
		filePath = filepath.Join(r.basePath, "mon_data", zone, filename)
	} else {
		filePath = filepath.Join(r.basePath, "mon_groups", groupID, "mon_data", zone, filename)
	}

	data, err := r.fs.ReadFile(filePath)
	if err != nil {
		return 0, fmt.Errorf("failed to read %s: %w", filePath, err)
	}

	valueStr := strings.TrimSpace(string(data))

	val, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse %s %q from %s: %w",
			filename, valueStr, filePath, err)
	}

	return val, nil
}

// readTasksPIDs reads PIDs from a group's tasks file.
func (r *resctrlPowerMeterImpl) readTasksPIDs(id string) ([]int, error) {
	tasksPath := filepath.Join(r.basePath, "mon_groups", id, "tasks")

	data, err := r.fs.ReadFile(tasksPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read tasks file %s: %w", tasksPath, err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var pids []int
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			r.logger.Warn("Skipping invalid PID in tasks file",
				"group", id, "value", line, "error", err)
			continue
		}
		pids = append(pids, pid)
	}

	return pids, nil
}

// writePIDs writes PIDs to a group's tasks file.
// The Linux resctrl tasks pseudo-file accepts only a limited buffer per
// write() syscall — writing many PIDs at once exceeds this limit and
// causes EINVAL.  We write one PID per syscall for reliability.
//
// Individual PID writes may fail (e.g., process exited between procfs scan
// and resctrl write) — these are logged and skipped. The function only
// returns an error if no PIDs could be written at all.
func (r *resctrlPowerMeterImpl) writePIDs(id string, pids []int) error {
	tasksPath := filepath.Join(r.basePath, "mon_groups", id, "tasks")

	var lastErr error
	written := 0
	for _, pid := range pids {
		data := []byte(strconv.Itoa(pid) + "\n")
		if err := r.fs.WriteFile(tasksPath, data, 0o600); err != nil {
			r.logger.Debug("Failed to write PID to resctrl tasks (process may have exited)",
				"group", id, "pid", pid, "error", err)
			lastErr = err
			continue
		}
		written++
	}

	if written == 0 && lastErr != nil {
		return fmt.Errorf("failed to write any PIDs to %s: %w", tasksPath, lastErr)
	}

	return nil
}
