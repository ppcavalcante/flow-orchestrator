// Package arena provides memory management utilities for efficient workflow execution.
// This wraps the internal arena system, providing real arena allocation with stats tracking.
package arena

import (
	internalarena "github.com/ppcavalcante/flow-orchestrator/internal/workflow/arena"
)

// Arena is a memory allocator that allocates memory in blocks to reduce GC pressure.
// It delegates to the internal arena implementation for actual memory allocation.
type Arena struct {
	internal *internalarena.Arena
}

// NewArena creates a new arena with the default block size.
func NewArena() *Arena {
	return &Arena{
		internal: internalarena.NewArena(),
	}
}

// NewArenaWithBlockSize creates a new arena with the specified block size.
func NewArenaWithBlockSize(blockSize int) *Arena {
	if blockSize <= 0 {
		blockSize = 8192
	}
	return &Arena{
		internal: internalarena.NewArenaWithBlockSize(blockSize),
	}
}

// Alloc allocates memory from the arena.
func (a *Arena) Alloc(size int) []byte {
	return a.internal.Alloc(size)
}

// AllocString allocates a string from the arena.
func (a *Arena) AllocString(s string) string {
	return a.internal.AllocString(s)
}

// Reset resets the arena, freeing all allocated memory.
func (a *Arena) Reset() {
	a.internal.Reset()
}

// Stats returns statistics about the arena.
func (a *Arena) Stats() map[string]int64 {
	return a.internal.Stats()
}

// StringPool provides string interning to reduce memory usage.
// It delegates to the internal StringPool for arena-backed allocation.
type StringPool struct {
	internal *internalarena.StringPool
}

// NewStringPool creates a new string pool using the specified arena.
func NewStringPool(arena *Arena) *StringPool {
	return &StringPool{
		internal: internalarena.NewStringPool(arena.internal),
	}
}

// Intern returns an interned version of the string.
// If the string is already in the pool, the existing instance is returned.
// Otherwise, the string is added to the pool and returned.
func (p *StringPool) Intern(s string) string {
	return p.internal.Intern(s)
}

// Reset clears the string pool.
func (p *StringPool) Reset() {
	p.internal.Reset()
}

// Stats returns statistics about the string pool.
func (p *StringPool) Stats() map[string]int64 {
	return p.internal.Stats()
}
