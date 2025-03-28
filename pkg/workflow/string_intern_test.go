package workflow

import (
	"testing"
	"time"
)

func TestNewStringInterner(t *testing.T) {
	interner := NewStringInterner()
	if interner == nil {
		t.Fatal("NewStringInterner returned nil")
	}
	if interner.intern == nil {
		t.Fatal("Internal interner is nil")
	}
}

func TestNewStringInternerWithCapacity(t *testing.T) {
	capacity := 100
	commonStringsCount := 10
	interner := NewStringInternerWithCapacity(capacity, commonStringsCount)

	if interner == nil {
		t.Fatal("NewStringInternerWithCapacity returned nil")
	}
	if interner.intern == nil {
		t.Fatal("Internal interner is nil")
	}
}

func TestStringInternerIntern(t *testing.T) {
	interner := NewStringInterner()

	// Test interning a string
	original := "test string"
	interned := interner.Intern(original)

	// The interned string should be equal to the original
	if interned != original {
		t.Errorf("Interned string %q is not equal to original %q", interned, original)
	}

	// Interning the same string again should return the same instance
	interned2 := interner.Intern(original)
	if &interned2 != &interned && interned2 != interned {
		t.Error("Interning the same string twice should return the same instance or equal value")
	}
}

func TestStringInternerInternBatch(t *testing.T) {
	interner := NewStringInterner()

	// Test interning a batch of strings
	originals := []string{"string1", "string2", "string3"}
	interned := interner.InternBatch(originals)

	// The interned strings should be equal to the originals
	if len(interned) != len(originals) {
		t.Errorf("Expected %d interned strings, got %d", len(originals), len(interned))
	}

	for i, original := range originals {
		if interned[i] != original {
			t.Errorf("Interned string %q at index %d is not equal to original %q",
				interned[i], i, original)
		}
	}
}

func TestStringInternerClear(t *testing.T) {
	interner := NewStringInterner()

	// Intern some strings
	interner.Intern("string1")
	interner.Intern("string2")

	// Clear the interner
	interner.Clear()

	// Get stats to verify it was cleared
	hits, misses, total := interner.GetStats()
	if total != 0 {
		t.Errorf("Expected 0 total strings after Clear, got %d", total)
	}

	// These might be reset or not depending on implementation
	t.Logf("After Clear: hits=%d, misses=%d, total=%d", hits, misses, total)
}

func TestStringInternerGetStats(t *testing.T) {
	interner := NewStringInterner()

	// Intern some strings
	interner.Intern("string1")
	interner.Intern("string2")
	interner.Intern("string1") // This should be a hit

	// Get stats
	hits, misses, total := interner.GetStats()

	// We should have at least 1 hit and 2 misses
	if hits < 1 {
		t.Errorf("Expected at least 1 hit, got %d", hits)
	}

	if total < 2 {
		t.Errorf("Expected at least 2 total operations, got %d", total)
	}

	t.Logf("Stats: hits=%d, misses=%d, total=%d", hits, misses, total)
}

func TestStringInternerGetDetailedStats(t *testing.T) {
	interner := NewStringInterner()

	// Intern some strings
	interner.Intern("string1")
	interner.Intern("string2")
	interner.Intern("string1") // This should be a hit

	// Get detailed stats
	stats := interner.GetDetailedStats()

	// Check that we have some stats
	if len(stats) == 0 {
		t.Error("Expected non-empty detailed stats")
	}

	// Log the stats for inspection
	for key, value := range stats {
		t.Logf("Stat %s: %d", key, value)
	}
}

func TestStringInternerCacheSize(t *testing.T) {
	interner := NewStringInterner()

	// Empty interner should have cache size 0 or some small number for pre-allocated common strings
	initialSize := interner.CacheSize()

	// Intern some strings
	interner.Intern("string1")
	interner.Intern("string2")

	// Cache size should have increased
	newSize := interner.CacheSize()
	if newSize <= initialSize {
		t.Errorf("Expected cache size to increase from %d, got %d", initialSize, newSize)
	}
}

func TestInternString(t *testing.T) {
	// Test the global intern function
	original := "global test string"
	interned := InternString(original)

	// The interned string should be equal to the original
	if interned != original {
		t.Errorf("Interned string %q is not equal to original %q", interned, original)
	}
}

func TestInternStringBatch(t *testing.T) {
	// Test the global intern batch function
	originals := []string{"global1", "global2", "global3"}
	interned := InternStringBatch(originals)

	// The interned strings should be equal to the originals
	if len(interned) != len(originals) {
		t.Errorf("Expected %d interned strings, got %d", len(originals), len(interned))
	}

	for i, original := range originals {
		if interned[i] != original {
			t.Errorf("Interned string %q at index %d is not equal to original %q",
				interned[i], i, original)
		}
	}
}

func TestGlobalStringInterner(t *testing.T) {
	// Get the global interner
	interner := GlobalStringInterner()

	if interner == nil {
		t.Fatal("GlobalStringInterner returned nil")
	}
	if interner.intern == nil {
		t.Fatal("Internal interner is nil")
	}

	// Test basic functionality
	original := "global interner test"
	interned := interner.Intern(original)

	if interned != original {
		t.Errorf("Interned string %q is not equal to original %q", interned, original)
	}
}

func TestConfigureGlobalStringInterner(t *testing.T) {
	// Configure the global interner
	capacity := 1000
	refreshInterval := time.Minute
	loadFactor := 0.75

	// This should not panic
	ConfigureGlobalStringInterner(capacity, refreshInterval, loadFactor)

	// Get the global interner and test it
	interner := GlobalStringInterner()

	// Test basic functionality after configuration
	original := "configured global test"
	interned := interner.Intern(original)

	if interned != original {
		t.Errorf("Interned string %q is not equal to original %q", interned, original)
	}
}
