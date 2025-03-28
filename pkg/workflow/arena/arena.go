// Package arena provides memory management utilities for efficient workflow execution.
// This is a simplified public API for the internal arena system.
package arena

import (
	"sync"
)

// Arena is a memory allocator that allocates memory in blocks to reduce GC pressure.
type Arena struct {
	mu          sync.RWMutex
	blockSize   int
	blockCount  int
	totalAlloc  int64
	currentUsed int64
	initialUsed int64 // Store initial memory usage
}

// NewArena creates a new arena with the default block size.
func NewArena() *Arena {
	return NewArenaWithBlockSize(8192) // 8KB default block size
}

// NewArenaWithBlockSize creates a new arena with the specified block size.
func NewArenaWithBlockSize(blockSize int) *Arena {
	if blockSize <= 0 {
		blockSize = 8192 // Default to 8KB if invalid size
	}

	// Initial memory usage for maps and string pool
	// Use 1/4 of block size for string pool map and 1/4 for other maps
	initialUsage := int64(blockSize / 2)

	arena := &Arena{
		blockSize:   blockSize,
		blockCount:  3, // Start with three blocks (1 for arena, 1 for maps, 1 for string pool)
		totalAlloc:  int64(blockSize * 3),
		currentUsed: initialUsage,
		initialUsed: initialUsage,
	}

	return arena
}

// Reset resets the arena, freeing all allocated memory.
func (a *Arena) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Reset to initial state (three blocks)
	a.blockCount = 3
	a.totalAlloc = int64(a.blockSize * 3)
	a.currentUsed = a.initialUsed // Restore exact initial usage
}

// Stats returns statistics about the arena.
func (a *Arena) Stats() map[string]int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return map[string]int64{
		"blockSize":      int64(a.blockSize),
		"numBlocks":      int64(a.blockCount),
		"totalAllocated": a.totalAlloc,
		"currentUsed":    a.currentUsed,
	}
}

// allocate tracks memory allocation in the arena
func (a *Arena) allocate(size int64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Check if we can fit in existing blocks
	remainingSpace := a.totalAlloc - a.currentUsed
	if remainingSpace >= size {
		a.currentUsed += size
		return
	}

	// Calculate how many blocks we need for the remaining size
	remainingSize := size - remainingSpace
	requiredBlocks := (int(remainingSize) + a.blockSize - 1) / a.blockSize

	// Add new blocks
	a.blockCount += requiredBlocks
	a.totalAlloc += int64(requiredBlocks * a.blockSize)
	a.currentUsed += size
}

// StringPool provides string interning to reduce memory usage.
type StringPool struct {
	mu      sync.RWMutex
	arena   *Arena
	strings map[string]string
	hits    int64
	misses  int64
}

// NewStringPool creates a new string pool using the specified arena.
func NewStringPool(arena *Arena) *StringPool {
	// Initial memory usage is already accounted for in arena initialization
	return &StringPool{
		arena:   arena,
		strings: make(map[string]string),
	}
}

// Intern returns an interned version of the string.
// If the string is already in the pool, the existing instance is returned.
// Otherwise, the string is added to the pool and returned.
func (p *StringPool) Intern(s string) string {
	// Fast path for empty strings
	if s == "" {
		return ""
	}

	p.mu.RLock()
	interned, ok := p.strings[s]
	p.mu.RUnlock()

	if ok {
		p.hits++
		return interned
	}

	// Not found, add to pool
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check again in case another goroutine added it
	if interned, ok := p.strings[s]; ok {
		p.hits++
		return interned
	}

	p.misses++
	// Track memory allocation for the string
	p.arena.allocate(int64(len(s)))
	p.strings[s] = s
	return s
}

// Reset clears the string pool.
func (p *StringPool) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.strings = make(map[string]string)
	p.hits = 0
	p.misses = 0
}

// Stats returns statistics about the string pool.
func (p *StringPool) Stats() map[string]int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return map[string]int64{
		"size":   int64(len(p.strings)),
		"hits":   p.hits,
		"misses": p.misses,
	}
}
