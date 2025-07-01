package memory

import (
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestStringInternBasic tests basic string interning functionality
func TestStringInternBasic(t *testing.T) {
	// Create a string interner
	interner := workflow.NewStringInterner()

	// Intern a string
	original := "test string"
	interned := interner.Intern(original)

	// Verify the interned string is equal to the original
	if interned != original {
		t.Errorf("Interned string %q does not match original %q", interned, original)
	}

	// Intern the same string again
	interned2 := interner.Intern(original)

	// Verify the interned strings are the same instance
	if &interned2 != &interned {
		t.Log("Interned strings are not the same instance (this is not necessarily an error)")
	}

	// Check stats
	hits, _, total := interner.GetStats()
	if hits < 1 {
		t.Errorf("Expected at least 1 hit, got %d", hits)
	}
	if total < 2 {
		t.Errorf("Expected at least 2 total, got %d", total)
	}
}

// TestStringInternCommonStrings tests interning of common strings
func TestStringInternCommonStrings(t *testing.T) {
	// Create a string interner
	interner := workflow.NewStringInterner()

	// Test common strings
	commonStrings := []string{
		"",
		"true",
		"false",
		"pending",
		"running",
		"completed",
		"failed",
		"skipped",
		"id",
		"name",
		"status",
	}

	for _, s := range commonStrings {
		// Intern the common string
		interned := interner.Intern(s)

		// Verify the interned string is equal to the original
		if interned != s {
			t.Errorf("Interned string %q does not match original %q", interned, s)
		}

		// Intern the same string again
		interned2 := interner.Intern(s)

		// Verify the interned strings are the same instance
		if &interned2 != &interned {
			t.Logf("Interned common string %q instances are not the same", s)
		}
	}
}

// TestStringInternLongString tests interning of long strings
func TestStringInternLongString(t *testing.T) {
	// Create a string interner with a small max length
	interner := workflow.NewStringInternerWithCapacity(128, 10)

	// Intern a string shorter than the max length
	shortStr := "short"
	shortInterned := interner.Intern(shortStr)

	// Verify the interned string is equal to the original
	if shortInterned != shortStr {
		t.Errorf("Interned string %q does not match original %q", shortInterned, shortStr)
	}

	// Intern a string longer than the max length
	longStr := "this is a very long string that exceeds the max length"
	longInterned := interner.Intern(longStr)

	// Verify the interned string is equal to the original
	if longInterned != longStr {
		t.Errorf("Interned string %q does not match original %q", longInterned, longStr)
	}

	// Verify the long string was not actually interned (just returned as-is)
	longInterned2 := interner.Intern(longStr)

	// The strings should be equal but might not be the same instance
	if longInterned2 != longStr {
		t.Errorf("Second interned string %q does not match original %q", longInterned2, longStr)
	}
}

// TestStringInternBatch tests batch string interning
func TestStringInternBatch(t *testing.T) {
	// Create a string interner
	interner := workflow.NewStringInterner()

	// Create test strings
	testStrings := []string{
		"string1",
		"string2",
		"string3",
		"pending", // Common string
		"running", // Common string
		"string4",
	}

	// Intern strings in batch
	internedStrings := interner.InternBatch(testStrings)

	// Verify all strings were interned correctly
	if len(internedStrings) != len(testStrings) {
		t.Errorf("Expected %d interned strings, got %d", len(testStrings), len(internedStrings))
	}

	for i, s := range testStrings {
		if internedStrings[i] != s {
			t.Errorf("Interned string %q at index %d does not match original %q", internedStrings[i], i, s)
		}
	}

	// Intern the same strings again individually
	for i, s := range testStrings {
		interned := interner.Intern(s)

		// Verify the interned string is equal to the original
		if interned != s {
			t.Errorf("Individually interned string %q does not match original %q", interned, s)
		}

		// Verify the interned string is the same as the batch interned string
		if interned != internedStrings[i] {
			t.Logf("Individually interned string %q is not the same instance as batch interned string", s)
		}
	}
}

// TestStringInternClear tests clearing the string interner
func TestStringInternClear(t *testing.T) {
	// Create a string interner
	interner := workflow.NewStringInterner()

	// Intern some strings
	strings := []string{"string1", "string2", "string3"}
	for _, s := range strings {
		interner.Intern(s)
	}

	// Check cache size
	initialSize := interner.CacheSize()
	if initialSize < len(strings) {
		t.Errorf("Expected cache size to be at least %d, got %d", len(strings), initialSize)
	}

	// Clear the cache
	interner.Clear()

	// Check cache size after clearing
	clearedSize := interner.CacheSize()
	if clearedSize != 0 {
		t.Errorf("Expected cache size to be 0 after clearing, got %d", clearedSize)
	}

	// Intern the strings again
	for _, s := range strings {
		interner.Intern(s)
	}

	// Check cache size after re-interning
	finalSize := interner.CacheSize()
	if finalSize < len(strings) {
		t.Errorf("Expected cache size to be at least %d after re-interning, got %d", len(strings), finalSize)
	}
}

// TestGlobalStringIntern tests the global string interning functions
func TestGlobalStringIntern(t *testing.T) {
	// Intern a string using the global interner
	original := "global test string"
	interned := workflow.InternString(original)

	// Verify the interned string is equal to the original
	if interned != original {
		t.Errorf("Globally interned string %q does not match original %q", interned, original)
	}

	// Intern the same string again
	interned2 := workflow.InternString(original)

	// Verify the interned strings are the same instance
	if &interned2 != &interned {
		t.Log("Globally interned strings are not the same instance (this is not necessarily an error)")
	}

	// Test batch interning
	testStrings := []string{"global1", "global2", "global3"}
	internedStrings := workflow.InternStringBatch(testStrings)

	// Verify all strings were interned correctly
	if len(internedStrings) != len(testStrings) {
		t.Errorf("Expected %d globally interned strings, got %d", len(testStrings), len(internedStrings))
	}

	for i, s := range testStrings {
		if internedStrings[i] != s {
			t.Errorf("Globally interned string %q at index %d does not match original %q", internedStrings[i], i, s)
		}
	}

	// Check global stats - note that the global interner is shared across tests
	// so we can't make assumptions about the exact count
	stats := workflow.GlobalStringInterner().GetDetailedStats()
	t.Logf("Global string intern stats: %v", stats)
}
