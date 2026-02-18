// SPDX-FileCopyrightText: 2026 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// mockDirEntry implements os.DirEntry for testing
type mockDirEntry struct {
	name  string
	isDir bool
}

func (m mockDirEntry) Name() string               { return m.name }
func (m mockDirEntry) IsDir() bool                 { return m.isDir }
func (m mockDirEntry) Type() fs.FileMode           { return 0 }
func (m mockDirEntry) Info() (fs.FileInfo, error)   { return nil, nil }

// mockFileInfo implements os.FileInfo for testing
type mockFileInfo struct {
	name  string
	isDir bool
}

func (m mockFileInfo) Name() string      { return m.name }
func (m mockFileInfo) Size() int64       { return 0 }
func (m mockFileInfo) Mode() fs.FileMode { return 0o644 }
func (m mockFileInfo) ModTime() time.Time { return time.Time{} }
func (m mockFileInfo) IsDir() bool       { return m.isDir }
func (m mockFileInfo) Sys() any          { return nil }

// mockResctrlFS is a mock resctrlFSReader for unit testing.
type mockResctrlFS struct {
	files      map[string]string    // path → contents
	dirs       map[string][]os.DirEntry
	statErr    map[string]error     // path → error from Stat
	readErr    map[string]error     // path → error from ReadFile
	mkdirErr   map[string]error     // path → error from MkdirAll
	removeErr  map[string]error     // path → error from RemoveAll
	writeErr   map[string]error     // path → error from WriteFile
	written    map[string]string    // tracks writes: path → data
	removed    []string             // tracks removed paths
	mkdirs     []string             // tracks created dirs
}

func newMockFS() *mockResctrlFS {
	return &mockResctrlFS{
		files:     make(map[string]string),
		dirs:      make(map[string][]os.DirEntry),
		statErr:   make(map[string]error),
		readErr:   make(map[string]error),
		mkdirErr:  make(map[string]error),
		removeErr: make(map[string]error),
		writeErr:  make(map[string]error),
		written:   make(map[string]string),
	}
}

func (m *mockResctrlFS) ReadFile(path string) ([]byte, error) {
	if err, ok := m.readErr[path]; ok {
		return nil, err
	}
	if data, ok := m.files[path]; ok {
		return []byte(data), nil
	}
	return nil, fmt.Errorf("file not found: %s", path)
}

func (m *mockResctrlFS) ReadDir(path string) ([]os.DirEntry, error) {
	if entries, ok := m.dirs[path]; ok {
		return entries, nil
	}
	return nil, fmt.Errorf("directory not found: %s", path)
}

func (m *mockResctrlFS) MkdirAll(path string, perm os.FileMode) error {
	m.mkdirs = append(m.mkdirs, path)
	if err, ok := m.mkdirErr[path]; ok {
		return err
	}
	return nil
}

func (m *mockResctrlFS) RemoveAll(path string) error {
	m.removed = append(m.removed, path)
	if err, ok := m.removeErr[path]; ok {
		return err
	}
	return nil
}

func (m *mockResctrlFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	// Return any injected write error without recording the write
	if err, ok := m.writeErr[path]; ok {
		return err
	}
	// Accumulate writes (writePIDs writes one PID per call)
	m.written[path] += string(data)
	return nil
}

func (m *mockResctrlFS) Stat(path string) (os.FileInfo, error) {
	if err, ok := m.statErr[path]; ok {
		return nil, err
	}
	// Check if it's a known file or dir
	if _, ok := m.files[path]; ok {
		return &mockFileInfo{name: path}, nil
	}
	if _, ok := m.dirs[path]; ok {
		return &mockFileInfo{name: path, isDir: true}, nil
	}
	return nil, fmt.Errorf("not found: %s", path)
}

func TestResctrlPowerMeterName(t *testing.T) {
	meter := NewResctrlPowerMeter()
	if meter.Name() != "resctrl" {
		t.Errorf("expected name 'resctrl', got %q", meter.Name())
	}
}

func TestResctrlInitWithAETZones(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	// Set up mon_data directory with two AET zones
	mockFS.dirs[basePath+"/mon_data"] = []os.DirEntry{
		mockDirEntry{name: "mon_PERF_PKG_0", isDir: true},
		mockDirEntry{name: "mon_PERF_PKG_1", isDir: true},
		mockDirEntry{name: "mon_L3_00", isDir: true}, // non-AET, should be skipped
	}

	// core_energy files exist in both AET zones
	mockFS.files[basePath+"/mon_data/mon_PERF_PKG_0/core_energy"] = "12345\n"
	mockFS.files[basePath+"/mon_data/mon_PERF_PKG_1/core_energy"] = "67890\n"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
		WithResctrlLogger(slog.Default()),
	)

	err := meter.Init()
	if err != nil {
		t.Fatalf("Init() failed: %v", err)
	}

	zones := meter.Zones()
	if len(zones) != 2 {
		t.Fatalf("expected 2 zones, got %d: %v", len(zones), zones)
	}
	if zones[0] != "mon_PERF_PKG_0" || zones[1] != "mon_PERF_PKG_1" {
		t.Errorf("unexpected zones: %v", zones)
	}
}

func TestResctrlInitNoAETZones(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	// mon_data exists but has no AET zones
	mockFS.dirs[basePath+"/mon_data"] = []os.DirEntry{
		mockDirEntry{name: "mon_L3_00", isDir: true},
	}

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	err := meter.Init()
	if err == nil {
		t.Fatal("Init() should have failed with no AET zones")
	}
	if !strings.Contains(err.Error(), "no AET core_energy zones") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResctrlInitNoMonData(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	err := meter.Init()
	if err == nil {
		t.Fatal("Init() should have failed when mon_data is missing")
	}
	if !strings.Contains(err.Error(), "not accessible") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResctrlInitZoneWithoutCoreEnergy(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	// mon_data with AET zone name but no core_energy file
	mockFS.dirs[basePath+"/mon_data"] = []os.DirEntry{
		mockDirEntry{name: "mon_PERF_PKG_0", isDir: true},
	}
	mockFS.statErr[basePath+"/mon_data/mon_PERF_PKG_0/core_energy"] = fmt.Errorf("not found")

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	err := meter.Init()
	if err == nil {
		t.Fatal("Init() should have failed with no valid AET zones")
	}
}

func TestReadGroupEnergyInteger(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	// Set up zones
	mockFS.dirs[basePath+"/mon_data"] = []os.DirEntry{
		mockDirEntry{name: "mon_PERF_PKG_0", isDir: true},
		mockDirEntry{name: "mon_PERF_PKG_1", isDir: true},
	}
	mockFS.files[basePath+"/mon_data/mon_PERF_PKG_0/core_energy"] = "0"
	mockFS.files[basePath+"/mon_data/mon_PERF_PKG_1/core_energy"] = "0"

	// Group energy files
	mockFS.files[basePath+"/mon_groups/test-pod/mon_data/mon_PERF_PKG_0/core_energy"] = "100000\n"
	mockFS.files[basePath+"/mon_groups/test-pod/mon_data/mon_PERF_PKG_1/core_energy"] = "200000\n"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	if err := meter.Init(); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}

	energy, err := meter.ReadGroupEnergy("test-pod")
	if err != nil {
		t.Fatalf("ReadGroupEnergy() failed: %v", err)
	}

	// Input values are in Joules; output is float64 Joules.
	// 100000 + 200000 = 300000 Joules
	expected := 300000.0
	if energy != expected {
		t.Errorf("expected energy %g, got %g", expected, energy)
	}
}

func TestReadGroupEnergyFloat(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	mockFS.dirs[basePath+"/mon_data"] = []os.DirEntry{
		mockDirEntry{name: "mon_PERF_PKG_0", isDir: true},
	}
	mockFS.files[basePath+"/mon_data/mon_PERF_PKG_0/core_energy"] = "0"

	// Float format energy value
	mockFS.files[basePath+"/mon_groups/test-pod/mon_data/mon_PERF_PKG_0/core_energy"] = "123456.789\n"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)
	if err := meter.Init(); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}

	energy, err := meter.ReadGroupEnergy("test-pod")
	if err != nil {
		t.Fatalf("ReadGroupEnergy() failed: %v", err)
	}

	// 123456.789 Joules → float64 Joules
	expected := 123456.789
	if energy != expected {
		t.Errorf("expected energy %g, got %g", expected, energy)
	}
}

func TestReadGroupEnergyMissingFile(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	mockFS.dirs[basePath+"/mon_data"] = []os.DirEntry{
		mockDirEntry{name: "mon_PERF_PKG_0", isDir: true},
	}
	mockFS.files[basePath+"/mon_data/mon_PERF_PKG_0/core_energy"] = "0"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)
	if err := meter.Init(); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}

	_, err := meter.ReadGroupEnergy("nonexistent-group")
	if err == nil {
		t.Fatal("ReadGroupEnergy() should fail for nonexistent group energy file")
	}
}

func TestReadGroupEnergyNoInit(t *testing.T) {
	meter := NewResctrlPowerMeter(
		WithResctrlFSReader(newMockFS()),
	)

	_, err := meter.ReadGroupEnergy("test")
	if err == nil {
		t.Fatal("ReadGroupEnergy() should fail when Init() hasn't been called")
	}
	if !strings.Contains(err.Error(), "no AET zones") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDiscoverGroupsUUID(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	podUUID := "12345678-1234-1234-1234-123456789abc"
	nonUUID := "my-custom-group"

	mockFS.dirs[basePath+"/mon_groups"] = []os.DirEntry{
		mockDirEntry{name: podUUID, isDir: true},
		mockDirEntry{name: nonUUID, isDir: true},
		mockDirEntry{name: "not-a-dir", isDir: false}, // regular file
	}

	// Tasks file for UUID group
	mockFS.files[basePath+"/mon_groups/"+podUUID+"/tasks"] = "1234\n5678\n"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	groups, err := meter.DiscoverGroups()
	if err != nil {
		t.Fatalf("DiscoverGroups() failed: %v", err)
	}

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}

	pids, ok := groups[podUUID]
	if !ok {
		t.Fatal("UUID group not found")
	}
	if len(pids) != 2 || pids[0] != 1234 || pids[1] != 5678 {
		t.Errorf("unexpected PIDs: %v", pids)
	}
}

func TestDiscoverGroupsNoMonGroups(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	_, err := meter.DiscoverGroups()
	if err == nil {
		t.Fatal("DiscoverGroups() should fail when mon_groups doesn't exist")
	}
}

func TestCreateMonitorGroup(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	err := meter.CreateMonitorGroup("test-pod", []int{100, 200})
	if err != nil {
		t.Fatalf("CreateMonitorGroup() failed: %v", err)
	}

	// Verify directory was created
	expectedDir := basePath + "/mon_groups/test-pod"
	if len(mockFS.mkdirs) == 0 || mockFS.mkdirs[0] != expectedDir {
		t.Errorf("expected mkdir at %s, got %v", expectedDir, mockFS.mkdirs)
	}

	// Verify PIDs were written
	expectedTasks := basePath + "/mon_groups/test-pod/tasks"
	written, ok := mockFS.written[expectedTasks]
	if !ok {
		t.Fatal("tasks file was not written")
	}
	if !strings.Contains(written, "100") || !strings.Contains(written, "200") {
		t.Errorf("PIDs not written correctly: %q", written)
	}
}

func TestCreateMonitorGroupMkdirFails(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"
	mockFS.mkdirErr[basePath+"/mon_groups/test-pod"] = fmt.Errorf("permission denied")

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	err := meter.CreateMonitorGroup("test-pod", []int{100})
	if err == nil {
		t.Fatal("CreateMonitorGroup() should fail when mkdir fails")
	}
}

func TestCreateMonitorGroupWritePIDsFails(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"
	// All writes to tasks file fail — no PIDs can be written
	mockFS.writeErr[basePath+"/mon_groups/test-pod/tasks"] = fmt.Errorf("no space")

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	err := meter.CreateMonitorGroup("test-pod", []int{100})
	if err == nil {
		t.Fatal("CreateMonitorGroup() should fail when all PID writes fail")
	}

	// Verify cleanup was attempted
	expectedRemoval := basePath + "/mon_groups/test-pod"
	found := false
	for _, p := range mockFS.removed {
		if p == expectedRemoval {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected cleanup after PID write failure")
	}
}

func TestDeleteMonitorGroup(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	err := meter.DeleteMonitorGroup("test-pod")
	if err != nil {
		t.Fatalf("DeleteMonitorGroup() failed: %v", err)
	}

	expectedRemoval := basePath + "/mon_groups/test-pod"
	if len(mockFS.removed) == 0 || mockFS.removed[0] != expectedRemoval {
		t.Errorf("expected removal of %s, got %v", expectedRemoval, mockFS.removed)
	}
}

func TestGroupExists(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	// Group exists
	mockFS.dirs[basePath+"/mon_groups/existing-pod"] = nil

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	if !meter.GroupExists("existing-pod") {
		t.Error("GroupExists() should return true for existing group")
	}

	if meter.GroupExists("nonexistent-pod") {
		t.Error("GroupExists() should return false for nonexistent group")
	}
}

func TestAddPIDsToGroup(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	err := meter.AddPIDsToGroup("test-pod", []int{300, 400})
	if err != nil {
		t.Fatalf("AddPIDsToGroup() failed: %v", err)
	}

	expectedTasks := basePath + "/mon_groups/test-pod/tasks"
	written := mockFS.written[expectedTasks]
	if !strings.Contains(written, "300") || !strings.Contains(written, "400") {
		t.Errorf("PIDs not written correctly: %q", written)
	}
}

func TestReadGroupEnergyParseError(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	mockFS.dirs[basePath+"/mon_data"] = []os.DirEntry{
		mockDirEntry{name: "mon_PERF_PKG_0", isDir: true},
	}
	mockFS.files[basePath+"/mon_data/mon_PERF_PKG_0/core_energy"] = "0"
	mockFS.files[basePath+"/mon_groups/test-pod/mon_data/mon_PERF_PKG_0/core_energy"] = "not-a-number\n"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)
	if err := meter.Init(); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}

	_, err := meter.ReadGroupEnergy("test-pod")
	if err == nil {
		t.Fatal("ReadGroupEnergy() should fail with unparseable value")
	}
	if !strings.Contains(err.Error(), "failed to parse") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDiscoverGroupsTasksReadError(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	podUUID := "12345678-1234-1234-1234-123456789abc"
	mockFS.dirs[basePath+"/mon_groups"] = []os.DirEntry{
		mockDirEntry{name: podUUID, isDir: true},
	}
	// No tasks file → will cause read error

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	groups, err := meter.DiscoverGroups()
	if err != nil {
		t.Fatalf("DiscoverGroups() should not fail: %v", err)
	}

	// Group should be kept with empty PID list despite tasks read error
	if len(groups) != 1 {
		t.Errorf("expected 1 group (kept with empty PIDs), got %d", len(groups))
	}
	if pids, ok := groups[podUUID]; !ok {
		t.Errorf("expected group %s to be present", podUUID)
	} else if len(pids) != 0 {
		t.Errorf("expected empty PID list, got %v", pids)
	}
}

func TestReadTasksPIDsEmptyFile(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	podUUID := "12345678-1234-1234-1234-123456789abc"
	mockFS.dirs[basePath+"/mon_groups"] = []os.DirEntry{
		mockDirEntry{name: podUUID, isDir: true},
	}
	mockFS.files[basePath+"/mon_groups/"+podUUID+"/tasks"] = "\n"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	)

	groups, err := meter.DiscoverGroups()
	if err != nil {
		t.Fatalf("DiscoverGroups() failed: %v", err)
	}

	pids := groups[podUUID]
	if len(pids) != 0 {
		t.Errorf("expected 0 PIDs for empty tasks, got %v", pids)
	}
}

func TestValidateGroupID(t *testing.T) {
	tests := []struct {
		id      string
		wantErr bool
	}{
		{"12345678-1234-1234-1234-123456789abc", false},
		{"simple-name", false},
		{"pod_1", false},
		{"", true},
		{"../escape", true},
		{"foo/bar", true},
		{"foo\\bar", true},
		{"..", true},
		{"mon_groups/../etc", true},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			err := validateGroupID(tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateGroupID(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
		})
	}
}

func TestCreateMonitorGroupPathTraversal(t *testing.T) {
	mockFS := newMockFS()
	basePath := "/test/resctrl"

	meter := NewResctrlPowerMeter(
		WithResctrlBasePath(basePath),
		WithResctrlFSReader(mockFS),
	).(*resctrlPowerMeterImpl)

	err := meter.CreateMonitorGroup("../escape", []int{100})
	if err == nil {
		t.Fatal("expected error for path traversal ID, got nil")
	}

	err = meter.DeleteMonitorGroup("foo/bar")
	if err == nil {
		t.Fatal("expected error for path separator ID, got nil")
	}
}
