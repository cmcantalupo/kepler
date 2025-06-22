// SPDX-FileCopyrightText: 2025 The Kepler Authors
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"fmt"
	"log/slog"
	"sync"
	"github.com/geopm/geopm/geopmdgo/geopmdgo"
)

// geopmEnergyZone represents a single energy zone using the GEOPM PIO interface.
type geopmEnergyZone struct {
	name      string
	index     int
	domain    int
	mu        sync.Mutex
}

// Name returns the name of the energy zone.
func (z *geopmEnergyZone) Name() string {
	return z.name
}

// Index returns the index of the energy zone.
func (z *geopmEnergyZone) Index() int {
	return z.index
}

// Path returns an empty string as GEOPM does not use sysfs paths.
func (z *geopmEnergyZone) Path() string {
	return ""
}

// Energy reads the energy consumed by the zone using the GEOPM PIO interface.
func (z *geopmEnergyZone) Energy() (Energy, error) {
	z.mu.Lock()
	defer z.mu.Unlock()

	value, err := geopmdgo.ReadSignal("CPU_ENERGY", z.domain, z.index)
	if err != nil {
		return 0, fmt.Errorf("failed to read energy for zone %s: %w", z.name, err)
	}
	return Energy(value), nil
}

// MaxEnergy returns 0 as GEOPM does not provide a maximum energy value.
func (z *geopmEnergyZone) MaxEnergy() Energy {
	return 0
}

// geopmCPUPowerMeter implements the CPUPowerMeter interface using the GEOPM PIO interface.
type geopmCPUPowerMeter struct {
	logger *slog.Logger
	zones  []EnergyZone
}

// NewGEOPMCPUPowerMeter creates a new CPU power meter using the GEOPM PIO interface.
func NewGEOPMCPUPowerMeter(logger *slog.Logger) (CPUPowerMeter, error) {
	// Initialize the logger if not provided.
	if logger == nil {
		logger = slog.Default().With("meter", "geopm-cpu-meter")
	}

	// Get the list of available signals.
	signals, err := geopmdgo.SignalNames()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve GEOPM signals: %w", err)
	}

	// Check if the "CPU_ENERGY" signal is available.
	found := false
	for _, signal := range signals {
		if signal == "CPU_ENERGY" {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("CPU_ENERGY signal not found in GEOPM signals")
	}

	// Determine the domain type for the CPU_ENERGY signal
	domainType, err := geopmdgo.SignalDomainType("CPU_ENERGY")
	if err != nil {
		return nil, fmt.Errorf("failed to determine domain type for CPU_ENERGY: %w", err)
	}

	// Query the number of domains for the CPU_ENERGY signal using the correct syntax
	numDomains, err := geopmdgo.NumDomain(domainType)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve number of domains for CPU_ENERGY: %w", err)
	}

	// Get the domain name for the CPU_ENERGY signal
	domainName, err := geopmdgo.DomainName(domainType)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve domain name for CPU_ENERGY: %w", err)
	}

	// Create an energy zone for each domain
	var zones []EnergyZone
	for i := 0; i < numDomains; i++ {
		zone := &geopmEnergyZone{
			name:      fmt.Sprintf("%s-%d", domainName, i),
			index:     i,
			domain:    domainType,
		}
		zones = append(zones, zone)
	}

	return &geopmCPUPowerMeter{
		logger: logger,
		zones:  zones,
	}, nil
}

// Name returns the name of the power meter.
func (m *geopmCPUPowerMeter) Name() string {
	return "geopm"
}

// Zones returns the list of energy zones for the power meter.
func (m *geopmCPUPowerMeter) Zones() ([]EnergyZone, error) {
	return m.zones, nil
}
