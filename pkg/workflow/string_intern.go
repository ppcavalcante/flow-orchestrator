package workflow

import (
	"time"

	internal "github.com/ppcavalcante/flow-orchestrator/internal/workflow"
)

// stringInterner provides string interning functionality to reduce memory usage
// by reusing identical strings. It is internal infrastructure for WorkflowData.
type stringInterner struct {
	intern *internal.StringInterner
}

// newStringInterner creates a new string interner.
func newStringInterner() *stringInterner {
	return &stringInterner{
		intern: internal.NewStringInterner(),
	}
}

// newStringInternerWithCapacity creates a new string interner with the specified capacity.
func newStringInternerWithCapacity(capacity int, commonStringsCount int) *stringInterner {
	return &stringInterner{
		intern: internal.NewStringInternerWithCapacity(capacity, commonStringsCount),
	}
}

// Intern returns an interned version of the input string.
func (s *stringInterner) Intern(str string) string {
	return s.intern.Intern(str)
}

// InternBatch interns a batch of strings.
func (s *stringInterner) InternBatch(strs []string) []string {
	return s.intern.InternBatch(strs)
}

// Clear clears the intern cache.
func (s *stringInterner) Clear() {
	s.intern.Clear()
}

// GetStats returns statistics about the interner.
func (s *stringInterner) GetStats() (hits, misses, total int) {
	return s.intern.GetStats()
}

// GetDetailedStats returns detailed statistics about the interner.
func (s *stringInterner) GetDetailedStats() map[string]int64 {
	return s.intern.GetDetailedStats()
}

// CacheSize returns the current size of the intern cache.
func (s *stringInterner) CacheSize() int {
	return s.intern.CacheSize()
}

// internString interns a string using the global interner.
func internString(s string) string {
	return internal.InternString(s)
}

// internStringBatch interns a batch of strings using the global interner.
func internStringBatch(strs []string) []string {
	return internal.InternStringBatch(strs)
}

// globalStringInterner returns the global string interner.
func globalStringInterner() *stringInterner {
	internalInterner := internal.GlobalStringInterner()
	return &stringInterner{
		intern: internalInterner,
	}
}

// configureGlobalStringInterner configures the global string interner.
func configureGlobalStringInterner(capacity int, refreshInterval time.Duration, loadFactor float64) {
	internal.ConfigureGlobalStringInterner(capacity, refreshInterval, loadFactor)
}
