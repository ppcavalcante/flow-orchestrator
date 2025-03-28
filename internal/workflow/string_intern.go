package workflow

import (
	"sync"
	"sync/atomic"
	"time"
)

// StringInterner provides efficient string interning to reduce string allocations
// by reusing identical string values. This is particularly useful for repeated strings
// like node names, status values, and common data keys.
type StringInterner struct {
	mu    sync.RWMutex
	cache map[string]string

	// Statistics for monitoring
	stats struct {
		hits   int64
		misses int64
		total  int64
	}

	// Common strings that are frequently used
	commonStrings map[string]string
}

// NewStringInterner creates a new string interner with default settings
func NewStringInterner() *StringInterner {
	return NewStringInternerWithCapacity(10000, 1024)
}

// NewStringInternerWithCapacity creates a new string interner with the specified capacity
func NewStringInternerWithCapacity(capacity int, _ int) *StringInterner {
	si := &StringInterner{
		cache: make(map[string]string, capacity),
		commonStrings: map[string]string{
			"":          "",
			"true":      "true",
			"false":     "false",
			"success":   "success",
			"failure":   "failure",
			"error":     "error",
			"completed": "completed",
			"running":   "running",
			"pending":   "pending",
			"failed":    "failed",
			"skipped":   "skipped",
		},
	}
	return si
}

// Intern returns an interned version of the input string
func (si *StringInterner) Intern(s string) string {
	atomic.AddInt64(&si.stats.total, 1)

	// Fast path for empty strings and common strings
	if s == "" {
		atomic.AddInt64(&si.stats.hits, 1)
		return ""
	}

	// Check common strings without locking
	if interned, ok := si.commonStrings[s]; ok {
		atomic.AddInt64(&si.stats.hits, 1)
		return interned
	}

	// Try read lock first for better concurrency
	si.mu.RLock()
	interned, ok := si.cache[s]
	si.mu.RUnlock()

	if ok {
		atomic.AddInt64(&si.stats.hits, 1)
		return interned
	}

	// Not found, acquire write lock and check again
	atomic.AddInt64(&si.stats.misses, 1)
	si.mu.Lock()
	defer si.mu.Unlock()

	// Double-check after acquiring write lock
	if interned, ok := si.cache[s]; ok {
		return interned
	}

	// Store the string in the cache
	// Use the original string as the interned version
	si.cache[s] = s
	return s
}

// InternBatch interns a batch of strings at once
func (si *StringInterner) InternBatch(strs []string) []string {
	if len(strs) == 0 {
		return strs
	}

	result := make([]string, len(strs))

	// First pass: try to intern strings without write locking
	needWriteLock := false
	for i, s := range strs {
		atomic.AddInt64(&si.stats.total, 1)

		// Fast path for empty strings and common strings
		if s == "" {
			atomic.AddInt64(&si.stats.hits, 1)
			result[i] = ""
			continue
		}

		// Check common strings without locking
		if interned, ok := si.commonStrings[s]; ok {
			atomic.AddInt64(&si.stats.hits, 1)
			result[i] = interned
			continue
		}

		// Try to find in cache with read lock
		si.mu.RLock()
		interned, ok := si.cache[s]
		si.mu.RUnlock()

		if ok {
			atomic.AddInt64(&si.stats.hits, 1)
			result[i] = interned
		} else {
			// Mark for second pass with write lock
			atomic.AddInt64(&si.stats.misses, 1)
			result[i] = ""
			needWriteLock = true
		}
	}

	// If all strings were interned without write lock, we're done
	if !needWriteLock {
		return result
	}

	// Second pass: handle strings that need to be added to the cache
	si.mu.Lock()
	defer si.mu.Unlock()

	for i, s := range strs {
		if result[i] == "" && s != "" {
			// Check again after acquiring write lock
			if interned, ok := si.cache[s]; ok {
				result[i] = interned
			} else {
				// Add to cache
				si.cache[s] = s
				result[i] = s
			}
		}
	}

	return result
}

// GetStats returns basic statistics about the interner
func (si *StringInterner) GetStats() (hits, misses, total int) {
	return int(atomic.LoadInt64(&si.stats.hits)),
		int(atomic.LoadInt64(&si.stats.misses)),
		int(atomic.LoadInt64(&si.stats.total))
}

// GetDetailedStats returns detailed statistics about the interner
func (si *StringInterner) GetDetailedStats() map[string]int64 {
	si.mu.RLock()
	cacheSize := len(si.cache)
	si.mu.RUnlock()

	return map[string]int64{
		"hits":      atomic.LoadInt64(&si.stats.hits),
		"misses":    atomic.LoadInt64(&si.stats.misses),
		"total":     atomic.LoadInt64(&si.stats.total),
		"cacheSize": int64(cacheSize),
	}
}

// CacheSize returns the current number of entries in the cache
func (si *StringInterner) CacheSize() int {
	si.mu.RLock()
	defer si.mu.RUnlock()
	return len(si.cache)
}

// Clear empties the cache
func (si *StringInterner) Clear() {
	si.mu.Lock()
	defer si.mu.Unlock()

	// Create a new map to replace the old one
	si.cache = make(map[string]string, len(si.cache))

	// Reset statistics
	atomic.StoreInt64(&si.stats.hits, 0)
	atomic.StoreInt64(&si.stats.misses, 0)
	atomic.StoreInt64(&si.stats.total, 0)
}

// Global string interner instance
var globalStringInterner = NewStringInterner()

// InternString interns a string using the global interner
func InternString(s string) string {
	return globalStringInterner.Intern(s)
}

// InternStringBatch interns a batch of strings using the global interner
func InternStringBatch(strs []string) []string {
	return globalStringInterner.InternBatch(strs)
}

// GlobalStringInterner returns the global string interner instance
func GlobalStringInterner() *StringInterner {
	return globalStringInterner
}

// ConfigureGlobalStringInterner configures the global string interner
func ConfigureGlobalStringInterner(_ int, _ time.Duration, _ float64) {
	// Implementation details
}
