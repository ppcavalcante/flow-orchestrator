package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestNewInstrumentedRWMutex(t *testing.T) {
	mutex := NewInstrumentedRWMutex()

	if mutex == nil {
		t.Fatal("NewInstrumentedRWMutex returned nil")
	}

	if mutex.config == nil {
		t.Error("InstrumentedRWMutex should have a non-nil config")
	}

	if mutex.name != "" {
		t.Errorf("InstrumentedRWMutex should have an empty name, got %s", mutex.name)
	}
}

func TestNewInstrumentedRWMutexWithName(t *testing.T) {
	name := "test-mutex"
	mutex := NewInstrumentedRWMutexWithName(name)

	if mutex == nil {
		t.Fatal("NewInstrumentedRWMutexWithName returned nil")
	}

	if mutex.config == nil {
		t.Error("InstrumentedRWMutex should have a non-nil config")
	}

	if mutex.name != name {
		t.Errorf("InstrumentedRWMutex should have name %s, got %s", name, mutex.name)
	}
}

func TestNewInstrumentedRWMutexWithConfig(t *testing.T) {
	name := "test-mutex"
	config := ProductionMetricsConfig()
	mutex := NewInstrumentedRWMutexWithConfig(name, config)

	if mutex == nil {
		t.Fatal("NewInstrumentedRWMutexWithConfig returned nil")
	}

	if mutex.config != config {
		t.Error("InstrumentedRWMutex should use the provided config")
	}

	if mutex.name != name {
		t.Errorf("InstrumentedRWMutex should have name %s, got %s", name, mutex.name)
	}

	// Test with nil config
	mutex = NewInstrumentedRWMutexWithConfig(name, nil)
	if mutex.config == nil {
		t.Error("InstrumentedRWMutex should have a non-nil config even when nil is passed")
	}
}

func TestInstrumentedRWMutexUpdateConfig(t *testing.T) {
	mutex := NewInstrumentedRWMutex()

	newConfig := ProductionMetricsConfig()
	mutex.UpdateConfig(newConfig)

	if mutex.config != newConfig {
		t.Error("UpdateConfig should update the mutex's config")
	}

	// Test with nil config
	mutex.UpdateConfig(nil)
	if mutex.config != newConfig {
		t.Error("UpdateConfig should not change the config when nil is passed")
	}
}

func TestInstrumentedRWMutexLockUnlock(t *testing.T) {
	// Reset metrics before test
	Reset()

	// Create a mutex with enabled metrics
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableLockContention = true
	mutex := NewInstrumentedRWMutexWithConfig("test", config)

	// Test Lock/Unlock
	mutex.Lock()
	mutex.Unlock()

	// Check that metrics were recorded
	lockStats := GetOperationStats(OpLockAcquire)
	if lockStats.Count != 1 {
		t.Errorf("Lock operation count should be 1, got %d", lockStats.Count)
	}

	unlockStats := GetOperationStats(OpLockRelease)
	if unlockStats.Count != 1 {
		t.Errorf("Unlock operation count should be 1, got %d", unlockStats.Count)
	}

	// Test with disabled metrics
	config.Enabled = false
	mutex.UpdateConfig(config)

	// Reset counters
	Reset()

	mutex.Lock()
	mutex.Unlock()

	// Check that no metrics were recorded
	lockStats = GetOperationStats(OpLockAcquire)
	if lockStats.Count != 0 {
		t.Errorf("Lock operation count should be 0 when disabled, got %d", lockStats.Count)
	}
}

func TestInstrumentedRWMutexRLockRUnlock(t *testing.T) {
	// Reset metrics before test
	Reset()

	// Create a mutex with enabled metrics
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableLockContention = true
	mutex := NewInstrumentedRWMutexWithConfig("test", config)

	// Test RLock/RUnlock
	mutex.RLock()
	mutex.RUnlock()

	// Check that metrics were recorded
	rlockStats := GetOperationStats(OpRLockAcquire)
	if rlockStats.Count != 1 {
		t.Errorf("RLock operation count should be 1, got %d", rlockStats.Count)
	}

	runlockStats := GetOperationStats(OpRLockRelease)
	if runlockStats.Count != 1 {
		t.Errorf("RUnlock operation count should be 1, got %d", runlockStats.Count)
	}

	// Test with disabled metrics
	config.Enabled = false
	mutex.UpdateConfig(config)

	// Reset counters
	Reset()

	mutex.RLock()
	mutex.RUnlock()

	// Check that no metrics were recorded
	rlockStats = GetOperationStats(OpRLockAcquire)
	if rlockStats.Count != 0 {
		t.Errorf("RLock operation count should be 0 when disabled, got %d", rlockStats.Count)
	}
}

func TestInstrumentedRWMutexContention(t *testing.T) {
	// Reset metrics before test
	Reset()

	// Create a mutex with enabled metrics
	config := DefaultMetricsConfig()
	config.Enabled = true
	config.SamplingRate = 1.0
	config.EnableLockContention = true
	mutex := NewInstrumentedRWMutexWithConfig("test", config)

	// Create contention by holding the lock in one goroutine and trying to acquire it in another
	var wg sync.WaitGroup
	wg.Add(2)

	// First goroutine acquires and holds the lock
	go func() {
		defer wg.Done()
		mutex.Lock()
		// Hold the lock for a while to ensure contention
		time.Sleep(20 * time.Millisecond)
		mutex.Unlock()
	}()

	// Give the first goroutine time to acquire the lock
	time.Sleep(5 * time.Millisecond)

	// Second goroutine tries to acquire the lock (will experience contention)
	go func() {
		defer wg.Done()
		mutex.Lock()
		mutex.Unlock()
	}()

	// Wait for both goroutines to complete
	wg.Wait()

	// Check that contention was recorded
	contentionStats := GetLockContentionStats()
	if contentionStats.Count == 0 {
		t.Error("Lock contention should have been recorded")
	}

	if contentionStats.TotalTimeNs < 10*1000*1000 { // At least 10ms
		t.Errorf("Contention time should be at least 10ms, got %s", FormatDuration(contentionStats.TotalTimeNs))
	}
}
