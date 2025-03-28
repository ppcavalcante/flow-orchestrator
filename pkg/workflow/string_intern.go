package workflow

import (
	"time"

	internal "github.com/pparaujo/flow-orchestrator/internal/workflow"
)

// StringInterner provides string interning functionality to reduce memory usage
// by reusing identical strings.
type StringInterner struct {
	intern *internal.StringInterner
}

// NewStringInterner creates a new string interner
func NewStringInterner() *StringInterner {
	return &StringInterner{
		intern: internal.NewStringInterner(),
	}
}

// NewStringInternerWithCapacity creates a new string interner with the specified capacity
func NewStringInternerWithCapacity(capacity int, commonStringsCount int) *StringInterner {
	return &StringInterner{
		intern: internal.NewStringInternerWithCapacity(capacity, commonStringsCount),
	}
}

// Intern returns an interned version of the input string
func (s *StringInterner) Intern(str string) string {
	return s.intern.Intern(str)
}

// InternBatch interns a batch of strings
func (s *StringInterner) InternBatch(strs []string) []string {
	return s.intern.InternBatch(strs)
}

// Clear clears the intern cache
func (s *StringInterner) Clear() {
	s.intern.Clear()
}

// GetStats returns statistics about the interner
func (s *StringInterner) GetStats() (hits, misses, total int) {
	return s.intern.GetStats()
}

// GetDetailedStats returns detailed statistics about the interner
func (s *StringInterner) GetDetailedStats() map[string]int64 {
	return s.intern.GetDetailedStats()
}

// CacheSize returns the current size of the intern cache
func (s *StringInterner) CacheSize() int {
	return s.intern.CacheSize()
}

// InternString interns a string using the global interner
func InternString(s string) string {
	return internal.InternString(s)
}

// InternStringBatch interns a batch of strings using the global interner
func InternStringBatch(strs []string) []string {
	return internal.InternStringBatch(strs)
}

// GlobalStringInterner returns the global string interner
func GlobalStringInterner() *StringInterner {
	internalInterner := internal.GlobalStringInterner()
	return &StringInterner{
		intern: internalInterner,
	}
}

// ConfigureGlobalStringInterner configures the global string interner
func ConfigureGlobalStringInterner(capacity int, refreshInterval time.Duration, loadFactor float64) {
	internal.ConfigureGlobalStringInterner(capacity, refreshInterval, loadFactor)
}
