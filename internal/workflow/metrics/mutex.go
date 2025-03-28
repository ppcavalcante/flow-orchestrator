package metrics

import (
	"sync"
	"time"
)

// InstrumentedRWMutex is a wrapper around sync.RWMutex that tracks lock contention
type InstrumentedRWMutex struct {
	mu     sync.RWMutex
	name   string
	config *MetricsConfig
}

// Lock acquires an exclusive lock and tracks contention
func (m *InstrumentedRWMutex) Lock() {
	// If metrics are disabled, just acquire the lock without instrumentation
	if m.config == nil || !m.config.ShouldCollectLockContention() {
		m.mu.Lock()
		return
	}

	// Start with a quick try to acquire the lock without blocking
	if m.mu.TryLock() {
		TrackOperation(OpLockAcquire, func() {})
		return
	}

	// If we couldn't acquire immediately, there's contention
	start := time.Now()
	TrackOperation(OpLockAcquire, func() {
		m.mu.Lock()
	})

	// Record contention time
	contentionTime := time.Since(start)
	if contentionTime > 10*time.Microsecond { // Threshold to consider it contention
		RecordLockContention(contentionTime)
	}
}

// Unlock releases an exclusive lock
func (m *InstrumentedRWMutex) Unlock() {
	// If metrics are disabled, just release the lock without instrumentation
	if m.config == nil || !m.config.ShouldCollectLockContention() {
		m.mu.Unlock()
		return
	}

	TrackOperation(OpLockRelease, func() {
		m.mu.Unlock()
	})
}

// RLock acquires a shared lock and tracks contention
func (m *InstrumentedRWMutex) RLock() {
	// If metrics are disabled, just acquire the lock without instrumentation
	if m.config == nil || !m.config.ShouldCollectLockContention() {
		m.mu.RLock()
		return
	}

	// Start with a quick try to acquire the lock without blocking
	if m.mu.TryRLock() {
		TrackOperation(OpRLockAcquire, func() {})
		return
	}

	// If we couldn't acquire immediately, there's contention
	start := time.Now()
	TrackOperation(OpRLockAcquire, func() {
		m.mu.RLock()
	})

	// Record contention time
	contentionTime := time.Since(start)
	if contentionTime > 10*time.Microsecond { // Threshold to consider it contention
		RecordLockContention(contentionTime)
	}
}

// RUnlock releases a shared lock
func (m *InstrumentedRWMutex) RUnlock() {
	// If metrics are disabled, just release the lock without instrumentation
	if m.config == nil || !m.config.ShouldCollectLockContention() {
		m.mu.RUnlock()
		return
	}

	TrackOperation(OpRLockRelease, func() {
		m.mu.RUnlock()
	})
}

// NewInstrumentedRWMutex creates a new instrumented RWMutex with the default configuration
func NewInstrumentedRWMutex() *InstrumentedRWMutex {
	return NewInstrumentedRWMutexWithConfig("", GetGlobalConfig())
}

// NewInstrumentedRWMutexWithName creates a new instrumented RWMutex with a name and the default configuration
func NewInstrumentedRWMutexWithName(name string) *InstrumentedRWMutex {
	return NewInstrumentedRWMutexWithConfig(name, GetGlobalConfig())
}

// NewInstrumentedRWMutexWithConfig creates a new instrumented RWMutex with the specified configuration
func NewInstrumentedRWMutexWithConfig(name string, config *MetricsConfig) *InstrumentedRWMutex {
	if config == nil {
		config = DefaultMetricsConfig()
	}

	return &InstrumentedRWMutex{
		name:   name,
		config: config,
	}
}

// UpdateConfig updates the metrics configuration for this mutex
func (m *InstrumentedRWMutex) UpdateConfig(config *MetricsConfig) {
	if config == nil {
		return
	}
	m.config = config
}
