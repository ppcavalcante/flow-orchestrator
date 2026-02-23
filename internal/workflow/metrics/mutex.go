package metrics

import (
	"sync"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/internal/workflow/utils"
)

// InstrumentedRWMutex is a wrapper around sync.RWMutex that tracks lock contention
type InstrumentedRWMutex struct {
	mu     sync.RWMutex
	name   string
	config *MetricsConfig
}

// shouldSample makes a single sampling decision for a lock operation.
// Returns true if this operation should be instrumented, false otherwise.
func (m *InstrumentedRWMutex) shouldSample() bool {
	if m.config == nil || !m.config.ShouldCollectLockContention() {
		return false
	}
	if m.config.SamplingRate < 1.0 && utils.SecureRandomFloat64() > m.config.SamplingRate {
		return false
	}
	return true
}

// Lock acquires an exclusive lock and tracks contention
func (m *InstrumentedRWMutex) Lock() {
	sampled := m.shouldSample()
	if !sampled {
		m.mu.Lock()
		return
	}

	// Start with a quick try to acquire the lock without blocking
	if m.mu.TryLock() {
		// Record the acquire with zero-cost operation
		RecordOperationTimingDirect(OpLockAcquire, 0)
		return
	}

	// If we couldn't acquire immediately, there's contention
	start := time.Now()
	m.mu.Lock()
	contentionTime := time.Since(start)

	// Record both timing and contention with the same sampling decision
	RecordOperationTimingDirect(OpLockAcquire, contentionTime)
	if contentionTime > 10*time.Microsecond {
		RecordLockContention(contentionTime)
	}
}

// Unlock releases an exclusive lock
func (m *InstrumentedRWMutex) Unlock() {
	sampled := m.shouldSample()
	if !sampled {
		m.mu.Unlock()
		return
	}

	start := time.Now()
	m.mu.Unlock()
	RecordOperationTimingDirect(OpLockRelease, time.Since(start))
}

// RLock acquires a shared lock and tracks contention
func (m *InstrumentedRWMutex) RLock() {
	sampled := m.shouldSample()
	if !sampled {
		m.mu.RLock()
		return
	}

	// Start with a quick try to acquire the lock without blocking
	if m.mu.TryRLock() {
		RecordOperationTimingDirect(OpRLockAcquire, 0)
		return
	}

	// If we couldn't acquire immediately, there's contention
	start := time.Now()
	m.mu.RLock()
	contentionTime := time.Since(start)

	// Record both timing and contention with the same sampling decision
	RecordOperationTimingDirect(OpRLockAcquire, contentionTime)
	if contentionTime > 10*time.Microsecond {
		RecordLockContention(contentionTime)
	}
}

// RUnlock releases a shared lock
func (m *InstrumentedRWMutex) RUnlock() {
	sampled := m.shouldSample()
	if !sampled {
		m.mu.RUnlock()
		return
	}

	start := time.Now()
	m.mu.RUnlock()
	RecordOperationTimingDirect(OpRLockRelease, time.Since(start))
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
