// SPDX-FileCopyrightText: 2025 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// TestGEOPMCPUPowerMeterInterface ensures that geopmCPUPowerMeter properly implements the CPUPowerMeter interface
func TestGEOPMCPUPowerMeterInterface(t *testing.T) {
	var _ CPUPowerMeter = (*geopmCPUPowerMeter)(nil)
}

func TestNewGEOPMCPUPowerMeter(t *testing.T) {
	mockGEOPM := &mockGEOPMReader{}
	mockGEOPM.On("SignalNames").Return([]string{"CPU_ENERGY"}, nil)
	mockGEOPM.On("SignalDomainType", "CPU_ENERGY").Return(1, nil)
	mockGEOPM.On("NumDomain", 1).Return(2, nil)
	mockGEOPM.On("DomainName", 1).Return("package", nil)
	mockGEOPM.On("ReadSignal", mock.Anything, mock.Anything, mock.Anything).Return(100.0, nil)

	meter, err := NewGEOPMCPUPowerMeter(nil)
	assert.NotNil(t, meter, "NewGEOPMCPUPowerMeter should not return nil")
	assert.NoError(t, err, "NewGEOPMCPUPowerMeter should not return error")
	assert.IsType(t, &geopmCPUPowerMeter{}, meter, "NewGEOPMCPUPowerMeter should return a *geopmCPUPowerMeter")
}

func TestGEOPMCPUPowerMeter_Name(t *testing.T) {
	meter := &geopmCPUPowerMeter{}
	name := meter.Name()
	assert.Equal(t, "geopm", name, "Name() should return 'geopm'")
}

func TestGEOPMCPUPowerMeter_Zones(t *testing.T) {
	mockGEOPM := &mockGEOPMReader{}
	mockGEOPM.On("SignalNames").Return([]string{"CPU_ENERGY"}, nil)
	mockGEOPM.On("SignalDomainType", "CPU_ENERGY").Return(1, nil)
	mockGEOPM.On("NumDomain", 1).Return(2, nil)
	mockGEOPM.On("DomainName", 1).Return("package", nil)
	mockGEOPM.On("Sample", mock.Anything).Return(100.0, nil)

	meter, err := NewGEOPMCPUPowerMeter(nil)
	assert.NoError(t, err, "NewGEOPMCPUPowerMeter should not return an error")

	zones, err := meter.Zones()
	assert.NoError(t, err, "Zones() should not return an error")
	assert.NotNil(t, zones, "Zones() should return a non-nil slice")
	assert.Equal(t, 2, len(zones), "Zones() should return the correct number of zones")

	for i, zone := range zones {
		assert.Equal(t, fmt.Sprintf("package-%d", i), zone.Name(), "Zone name should match expected format")
		energy, err := zone.Energy()
		assert.NoError(t, err, "Energy() should not return an error")
		assert.Greater(t, energy, Energy(0), "Energy() should return a positive value")
	}
}

// mockGEOPMReader is a mock implementation of the GEOPM PIO interface
type mockGEOPMReader struct {
	mock.Mock
}

func (m *mockGEOPMReader) SignalNames() ([]string, error) {
	args := m.Called()
	return args.Get(0).([]string), args.Error(1)
}

func (m *mockGEOPMReader) SignalDomainType(signal string) (int, error) {
	args := m.Called(signal)
	return args.Int(0), args.Error(1)
}

func (m *mockGEOPMReader) NumDomain(signalIdx int) (int, error) {
	args := m.Called(signalIdx)
	return args.Int(0), args.Error(1)
}

func (m *mockGEOPMReader) DomainName(domainIdx int) (string, error) {
	args := m.Called(domainIdx)
	return args.String(0), args.Error(1)
}

func (m *mockGEOPMReader) Sample(signalIdx int) (float64, error) {
	args := m.Called(signalIdx)
	return args.Get(0).(float64), args.Error(1)
}
