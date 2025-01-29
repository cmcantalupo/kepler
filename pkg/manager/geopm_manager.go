package manager

import (
    "github.com/intel/geopm/geopmdgo"
    "github.com/sustainable-computing-io/kepler/pkg/metrics"
    "k8s.io/klog/v2"
)

type GEOPMManager struct {
    signalIndices map[string]int
    signals       []string
}

func NewGEOPMManager() (*Manager, error) {
    klog.Infof("Initializing GEOPMManager")

    // Define the signals you want to collect
    signals := []string{"CPU_ENERGY", "GPU_ENERGY", "DRAM_ENERGY"}

    // Initialize GEOPMManager
    geopmManager := &GEOPMManager{
        signalIndices: make(map[string]int),
        signals:       []string{},
    }

    // Push signals to GEOPM PlatformIO
    for _, signal := range signals {
        index, err := geopmdgo.PushSignal(signal, geopmdgo.DOMAIN_BOARD, 0)
        if err == nil {
            geopmManager.signalIndices[signal] = index
            geopmManager.signals = append(geopmManager.signals, signal)
	}
    }

    return &Manager{
        Collector: geopmManager,
    }, nil
}

// Start starts the telemetry collection
func (m *GEOPMManager) Start() error {
    klog.Infof("Starting GEOPMManager")
    return nil
}

// Stop stops the telemetry collection
func (m *GEOPMManager) Stop() error {
    klog.Infof("Stopping GEOPMManager")
    return nil
}

// Collect collects the telemetry data
func (m *GEOPMManager) Collect() ([]metrics.Sample, error) {
    klog.Infof("Collecting telemetry data")

    // Read batch of signals
    if err := geopmdgo.ReadBatch(); err != nil {
        return nil, err
    }

    // Collect samples
    samples := []metrics.Sample{}
    for _, signal := range m.signals {
        index := m.signalIndices[signal]
        value, err := geopmdgo.Sample(index)
        if err != nil {
            return nil, err
        }
        samples = append(samples, metrics.Sample{
            Name:  signal,
            Value: value,
        })
    }

    return samples, nil
}
