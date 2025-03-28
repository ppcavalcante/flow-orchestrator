package arena

import (
	"runtime"
	"sync"
)

// Arena provides a simple memory arena for efficient allocation and bulk deallocation
type Arena struct {
	// Blocks of memory
	blocks [][]byte

	// Current block and offset
	currentBlock int
	offset       int

	// Block size for new allocations
	blockSize int

	// Mutex for thread safety
	mu sync.Mutex

	// Statistics
	stats struct {
		totalAllocated int64
		totalBlocks    int
		wastedBytes    int64
	}
}

// NewArena creates a new memory arena with the default block size
func NewArena() *Arena {
	return NewArenaWithBlockSize(64 * 1024) // 64KB default block size
}

// NewArenaWithBlockSize creates a new memory arena with the specified block size
func NewArenaWithBlockSize(blockSize int) *Arena {
	// Allocate first block
	initialBlock := make([]byte, blockSize)

	return &Arena{
		blocks:       [][]byte{initialBlock},
		currentBlock: 0,
		offset:       0,
		blockSize:    blockSize,
		stats: struct {
			totalAllocated int64
			totalBlocks    int
			wastedBytes    int64
		}{
			totalBlocks: 1, // Start with 1 block
		},
	}
}

// Alloc allocates memory from the arena
func (a *Arena) Alloc(size int) []byte {
	// Handle zero-size allocation
	if size == 0 {
		return nil
	}

	// Align size to 8-byte boundary for better memory access
	alignedSize := (size + 7) & ^7

	a.mu.Lock()
	defer a.mu.Unlock()

	// Check if there's enough space in the current block
	if a.offset+alignedSize > len(a.blocks[a.currentBlock]) {
		// Not enough space, allocate a new block
		newBlockSize := a.blockSize
		if alignedSize > newBlockSize {
			// If the requested size is larger than our standard block size,
			// allocate a block large enough for this specific allocation
			newBlockSize = alignedSize
		}

		newBlock := make([]byte, newBlockSize)
		a.blocks = append(a.blocks, newBlock)
		a.currentBlock++
		a.offset = 0
		a.stats.totalBlocks++
	}

	// Allocate from the current block
	mem := a.blocks[a.currentBlock][a.offset : a.offset+alignedSize]
	a.offset += alignedSize
	a.stats.totalAllocated += int64(size)
	a.stats.wastedBytes += int64(alignedSize - size)

	return mem
}

// AllocString allocates a string from the arena
func (a *Arena) AllocString(s string) string {
	if s == "" {
		return ""
	}

	// Allocate memory for the string
	mem := a.Alloc(len(s))

	// Copy the string data
	copy(mem, s)

	// Convert back to string
	// We need to create a proper string header to avoid null bytes
	return string(mem[:len(s)])
}

// Reset resets the arena, allowing all memory to be reused
func (a *Arena) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Reset to the first block
	a.currentBlock = 0
	a.offset = 0

	// Clear statistics
	a.stats.totalAllocated = 0
	a.stats.wastedBytes = 0
}

// Free releases all memory held by the arena
func (a *Arena) Free() {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Clear all blocks to allow garbage collection
	a.blocks = nil
	a.currentBlock = 0
	a.offset = 0

	// Suggest garbage collection
	runtime.GC()

	// Reinitialize with a fresh block
	initialBlock := make([]byte, a.blockSize)
	a.blocks = [][]byte{initialBlock}

	// Clear statistics
	a.stats.totalAllocated = 0
	a.stats.totalBlocks = 1
	a.stats.wastedBytes = 0
}

// Stats returns statistics about the arena
func (a *Arena) Stats() map[string]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	return map[string]int64{
		"totalAllocated": a.stats.totalAllocated,
		"totalBlocks":    int64(a.stats.totalBlocks),
		"currentBlock":   int64(a.currentBlock),
		"wastedBytes":    a.stats.wastedBytes,
		"usedBytes":      int64(a.currentBlock*a.blockSize + a.offset),
		"blockSize":      int64(a.blockSize),
	}
}

// StringPool provides string interning within an arena
type StringPool struct {
	arena *Arena
	pool  map[string]string
	mu    sync.RWMutex

	// Common strings that are frequently used
	commonStrings map[string]string

	// Statistics
	stats struct {
		hits   int64
		misses int64
		total  int64
	}
	statsMu sync.Mutex // Mutex to protect stats field
}

// NewStringPool creates a new string pool using the provided arena
func NewStringPool(arena *Arena) *StringPool {
	// Initialize common strings
	commonStrings := map[string]string{
		"":          "",
		"true":      "true",
		"false":     "false",
		"success":   "success",
		"failure":   "failure",
		"pending":   "pending",
		"running":   "running",
		"error":     "error",
		"skipped":   "skipped",
		"id":        "id",
		"name":      "name",
		"status":    "status",
		"result":    "result",
		"data":      "data",
		"input":     "input",
		"output":    "output",
		"node":      "node",
		"edge":      "edge",
		"graph":     "graph",
		"workflow":  "workflow",
		"completed": "completed",
		"failed":    "failed",
	}

	return &StringPool{
		arena:         arena,
		pool:          make(map[string]string),
		commonStrings: commonStrings,
	}
}

// Intern interns a string in the pool
func (p *StringPool) Intern(s string) string {
	p.statsMu.Lock()
	p.stats.total++
	p.statsMu.Unlock()

	// Fast path for empty strings
	if s == "" {
		p.statsMu.Lock()
		p.stats.hits++
		p.statsMu.Unlock()
		return ""
	}

	// Check common strings without locking
	if interned, ok := p.commonStrings[s]; ok {
		p.statsMu.Lock()
		p.stats.hits++
		p.statsMu.Unlock()
		return interned
	}

	// Try read lock first
	p.mu.RLock()
	pooled, ok := p.pool[s]
	p.mu.RUnlock()

	if ok {
		p.statsMu.Lock()
		p.stats.hits++
		p.statsMu.Unlock()
		return pooled
	}

	// Not found, acquire write lock
	p.statsMu.Lock()
	p.stats.misses++
	p.statsMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Check again under write lock
	if pooled, ok := p.pool[s]; ok {
		return pooled
	}

	// Allocate in the arena
	pooled = p.arena.AllocString(s)
	p.pool[pooled] = pooled

	return pooled
}

// InternBatch interns multiple strings at once
func (p *StringPool) InternBatch(strs []string) []string {
	if len(strs) == 0 {
		return strs
	}

	result := make([]string, len(strs))

	// First pass: try to intern strings without write locking
	needWriteLock := false
	for i, s := range strs {
		p.statsMu.Lock()
		p.stats.total++
		p.statsMu.Unlock()

		// Fast path for empty strings
		if s == "" {
			p.statsMu.Lock()
			p.stats.hits++
			p.statsMu.Unlock()
			result[i] = ""
			continue
		}

		// Check common strings without locking
		if interned, ok := p.commonStrings[s]; ok {
			p.statsMu.Lock()
			p.stats.hits++
			p.statsMu.Unlock()
			result[i] = interned
			continue
		}

		// Try to find in pool with read lock
		p.mu.RLock()
		pooled, ok := p.pool[s]
		p.mu.RUnlock()

		if ok {
			p.statsMu.Lock()
			p.stats.hits++
			p.statsMu.Unlock()
			result[i] = pooled
		} else {
			// Mark for second pass with write lock
			p.statsMu.Lock()
			p.stats.misses++
			p.statsMu.Unlock()
			result[i] = ""
			needWriteLock = true
		}
	}

	// If all strings were interned without write lock, we're done
	if !needWriteLock {
		return result
	}

	// Second pass: handle strings that need to be added to the pool
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, s := range strs {
		if result[i] == "" && s != "" {
			// Check again after acquiring write lock
			if pooled, ok := p.pool[s]; ok {
				result[i] = pooled
			} else {
				// Allocate in the arena
				pooled = p.arena.AllocString(s)
				p.pool[pooled] = pooled
				result[i] = pooled
			}
		}
	}

	return result
}

// Stats returns statistics about the string pool
func (p *StringPool) Stats() map[string]int64 {
	p.mu.RLock()
	poolSize := len(p.pool)
	p.mu.RUnlock()

	p.statsMu.Lock()
	hits := p.stats.hits
	misses := p.stats.misses
	total := p.stats.total
	p.statsMu.Unlock()

	return map[string]int64{
		"hits":     hits,
		"misses":   misses,
		"total":    total,
		"poolSize": int64(poolSize),
	}
}

// Reset resets the string pool
func (p *StringPool) Reset() {
	p.mu.Lock()
	p.pool = make(map[string]string)
	p.mu.Unlock()

	p.statsMu.Lock()
	p.stats.hits = 0
	p.stats.misses = 0
	p.stats.total = 0
	p.statsMu.Unlock()
}
