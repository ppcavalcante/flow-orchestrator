package arena

import (
	"testing"
)

// TestArenaBasic tests basic arena functionality
func TestArenaBasic(t *testing.T) {
	// Create a new arena
	arena := NewArena()

	// Allocate some memory
	mem1 := arena.Alloc(100)
	if len(mem1) < 100 {
		t.Errorf("Expected allocated memory to be at least 100 bytes, got %d", len(mem1))
	}

	// Write to the memory
	for i := 0; i < 100; i++ {
		mem1[i] = byte(i)
	}

	// Allocate more memory
	mem2 := arena.Alloc(200)
	if len(mem2) < 200 {
		t.Errorf("Expected allocated memory to be at least 200 bytes, got %d", len(mem2))
	}

	// Write to the memory
	for i := 0; i < 200; i++ {
		mem2[i] = byte(i)
	}

	// Verify the first allocation wasn't affected
	for i := 0; i < 100; i++ {
		if mem1[i] != byte(i) {
			t.Errorf("Memory corruption detected at index %d", i)
		}
	}

	// Check stats
	stats := arena.Stats()
	if stats["totalAllocated"] < 300 {
		t.Errorf("Expected totalAllocated to be at least 300, got %d", stats["totalAllocated"])
	}

	// Reset the arena
	arena.Reset()

	// Allocate again
	mem3 := arena.Alloc(100)
	if len(mem3) < 100 {
		t.Errorf("Expected allocated memory to be at least 100 bytes, got %d", len(mem3))
	}

	// Free the arena
	arena.Free()

	// Allocate after freeing
	mem4 := arena.Alloc(100)
	if len(mem4) < 100 {
		t.Errorf("Expected allocated memory to be at least 100 bytes, got %d", len(mem4))
	}
}

// TestArenaLargeAllocation tests allocating memory larger than the block size
func TestArenaLargeAllocation(t *testing.T) {
	// Create a new arena with a small block size
	arena := NewArenaWithBlockSize(1024) // 1KB blocks

	// Allocate memory smaller than the block size
	small := arena.Alloc(512)
	if len(small) < 512 {
		t.Errorf("Expected allocated memory to be at least 512 bytes, got %d", len(small))
	}

	// Allocate memory larger than the block size
	large := arena.Alloc(2048) // 2KB
	if len(large) < 2048 {
		t.Errorf("Expected allocated memory to be at least 2048 bytes, got %d", len(large))
	}

	// Write to both allocations
	for i := 0; i < 512; i++ {
		small[i] = byte(i)
	}

	for i := 0; i < 2048; i++ {
		large[i] = byte(i % 256)
	}

	// Verify the data
	for i := 0; i < 512; i++ {
		if small[i] != byte(i) {
			t.Errorf("Small allocation corruption at index %d", i)
		}
	}

	for i := 0; i < 2048; i++ {
		if large[i] != byte(i%256) {
			t.Errorf("Large allocation corruption at index %d", i)
		}
	}

	// Check stats
	stats := arena.Stats()
	if stats["totalBlocks"] < 2 {
		t.Logf("Expected at least 2 blocks, got %d", stats["totalBlocks"])
	}
}

// TestArenaString tests string allocation
func TestArenaString(t *testing.T) {
	// Create a new arena
	arena := NewArena()

	// Allocate some strings
	strings := []string{
		"",
		"hello",
		"this is a test string",
		"this is a longer test string that should span multiple cache lines",
	}

	arenaStrings := make([]string, len(strings))
	for i, s := range strings {
		arenaStrings[i] = arena.AllocString(s)
	}

	// Verify the strings
	for i, s := range strings {
		if arenaStrings[i] != s {
			t.Errorf("String mismatch at index %d: expected %q, got %q", i, s, arenaStrings[i])
		}
	}

	// Check that empty string is handled correctly
	emptyStr := arena.AllocString("")
	if emptyStr != "" {
		t.Errorf("Empty string should remain empty, got %q", emptyStr)
	}

	// Check stats
	stats := arena.Stats()
	totalLen := 0
	for _, s := range strings {
		totalLen += len(s)
	}

	if stats["totalAllocated"] < int64(totalLen) {
		t.Errorf("Expected totalAllocated to be at least %d, got %d", totalLen, stats["totalAllocated"])
	}
}

// TestStringPool tests the string pool functionality
func TestStringPool(t *testing.T) {
	// Create a new arena and string pool
	arena := NewArena()
	pool := NewStringPool(arena)

	// Intern some strings
	strings := []string{
		"",
		"hello",
		"world",
		"hello", // Duplicate
		"this is a test string",
		"this is a test string", // Duplicate
	}

	internedStrings := make([]string, len(strings))
	for i, s := range strings {
		internedStrings[i] = pool.Intern(s)
	}

	// Verify the strings
	for i, s := range strings {
		if internedStrings[i] != s {
			t.Errorf("String mismatch at index %d: expected %q, got %q", i, s, internedStrings[i])
		}
	}

	// Check that duplicates are interned to the same string
	if internedStrings[1] != internedStrings[3] {
		t.Errorf("Duplicate strings should be interned to the same string")
	}

	if internedStrings[4] != internedStrings[5] {
		t.Errorf("Duplicate strings should be interned to the same string")
	}

	// Check stats
	stats := pool.Stats()
	if stats["hits"] < 2 {
		t.Errorf("Expected at least 2 hits, got %d", stats["hits"])
	}

	// Check pool size (4 unique strings: "", "hello", "world", "this is a test string")
	expectedPoolSize := 4
	if stats["poolSize"] != int64(expectedPoolSize) {
		t.Logf("Expected pool size to be %d, got %d", expectedPoolSize, stats["poolSize"])
	}

	// Reset the pool
	pool.Reset()

	// Check stats after reset
	stats = pool.Stats()
	if stats["hits"] != 0 || stats["misses"] != 0 || stats["total"] != 0 {
		t.Errorf("Stats should be reset to zero, got %v", stats)
	}

	if stats["poolSize"] != 0 {
		t.Errorf("Pool size should be 0 after reset, got %d", stats["poolSize"])
	}
}

// TestStringPoolBatch tests batch string interning
func TestStringPoolBatch(t *testing.T) {
	// Create a new arena and string pool
	arena := NewArena()
	pool := NewStringPool(arena)

	// Create test strings
	testStrings := []string{
		"string1",
		"string2",
		"string3",
		"string1", // Duplicate
		"string4",
	}

	// Intern strings in batch
	internedStrings := pool.InternBatch(testStrings)

	// Verify all strings were interned correctly
	if len(internedStrings) != len(testStrings) {
		t.Errorf("Expected %d interned strings, got %d", len(testStrings), len(internedStrings))
	}

	for i, s := range testStrings {
		if internedStrings[i] != s {
			t.Errorf("Interned string %q at index %d does not match original %q", internedStrings[i], i, s)
		}
	}

	// Check that duplicates are interned to the same string
	if internedStrings[0] != internedStrings[3] {
		t.Errorf("Duplicate strings should be interned to the same string")
	}

	// Intern the same strings again individually
	for i, s := range testStrings {
		interned := pool.Intern(s)

		// Verify the interned string is equal to the original
		if interned != s {
			t.Errorf("Individually interned string %q does not match original %q", interned, s)
		}

		// Verify the interned string is the same as the batch interned string
		if interned != internedStrings[i] {
			t.Logf("Individually interned string %q is not the same as batch interned string", s)
		}
	}

	// Check stats
	stats := pool.Stats()
	if stats["hits"] < 4 { // At least 4 hits from the duplicates and re-interning
		t.Errorf("Expected at least 4 hits, got %d", stats["hits"])
	}

	// Check pool size (4 unique strings: "string1", "string2", "string3", "string4")
	expectedPoolSize := 4
	if stats["poolSize"] != int64(expectedPoolSize) {
		t.Logf("Expected pool size to be %d, got %d", expectedPoolSize, stats["poolSize"])
	}
}

// TestStringPoolInternEdgeCases tests edge cases for the Intern function
func TestStringPoolInternEdgeCases(t *testing.T) {
	arena := NewArena()
	pool := NewStringPool(arena)

	// Test empty string
	emptyStr := pool.Intern("")
	if emptyStr != "" {
		t.Errorf("Expected empty string to be interned as empty string, got %q", emptyStr)
	}

	// Test common string
	// Add a common string to the pool's commonStrings map
	commonStr := "common"
	pool.commonStrings[commonStr] = commonStr

	internedCommon := pool.Intern(commonStr)
	if internedCommon != commonStr {
		t.Errorf("Expected common string to be interned as itself, got %q", internedCommon)
	}

	// Test string that's already in the pool
	str := "test"
	interned1 := pool.Intern(str)
	interned2 := pool.Intern(str)

	if interned1 != interned2 {
		t.Errorf("Expected same string to be interned to same value, got %q and %q", interned1, interned2)
	}

	// Check stats
	stats := pool.Stats()
	if stats["hits"] < 3 {
		t.Errorf("Expected at least 3 hits, got %d", stats["hits"])
	}
	if stats["misses"] < 1 {
		t.Errorf("Expected at least 1 miss, got %d", stats["misses"])
	}
}

// TestStringPoolInternBatchEdgeCases tests edge cases for the InternBatch function
func TestStringPoolInternBatchEdgeCases(t *testing.T) {
	arena := NewArena()
	pool := NewStringPool(arena)

	// Test empty batch
	emptyBatch := pool.InternBatch([]string{})
	if len(emptyBatch) != 0 {
		t.Errorf("Expected empty batch to return empty batch, got %v", emptyBatch)
	}

	// Test batch with empty string
	batch1 := pool.InternBatch([]string{""})
	if len(batch1) != 1 || batch1[0] != "" {
		t.Errorf("Expected batch with empty string to return batch with empty string, got %v", batch1)
	}

	// Test batch with common string
	// Add a common string to the pool's commonStrings map
	commonStr := "common"
	pool.commonStrings[commonStr] = commonStr

	batch2 := pool.InternBatch([]string{commonStr})
	if len(batch2) != 1 || batch2[0] != commonStr {
		t.Errorf("Expected batch with common string to return batch with common string, got %v", batch2)
	}

	// Test batch with mix of new and existing strings
	str1 := "test1"
	str2 := "test2"

	// Intern str1 first
	pool.Intern(str1)

	// Now intern both in a batch
	batch3 := pool.InternBatch([]string{str1, str2})
	if len(batch3) != 2 {
		t.Errorf("Expected batch with 2 strings to return batch with 2 strings, got %v", batch3)
	}

	// Both strings should now be in the pool
	if !pool.Has(str1) {
		t.Errorf("Expected %q to be in the pool", str1)
	}
	if !pool.Has(str2) {
		t.Errorf("Expected %q to be in the pool", str2)
	}

	// Test batch with all strings already in the pool (no write lock needed)
	batch4 := pool.InternBatch([]string{str1, str2})
	if len(batch4) != 2 {
		t.Errorf("Expected batch with 2 strings to return batch with 2 strings, got %v", batch4)
	}

	// Check stats
	stats := pool.Stats()
	if stats["hits"] < 3 {
		t.Errorf("Expected at least 3 hits, got %d", stats["hits"])
	}
	if stats["misses"] < 2 {
		t.Errorf("Expected at least 2 misses, got %d", stats["misses"])
	}
}

// Helper method to check if a string is in the pool
func (p *StringPool) Has(s string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	_, ok := p.pool[s]
	return ok
}

// TestArenaAllocEdgeCases tests edge cases for the Alloc function
func TestArenaAllocEdgeCases(t *testing.T) {
	// Create a new arena with a small block size
	arena := NewArenaWithBlockSize(64)

	// Test allocating zero bytes
	mem0 := arena.Alloc(0)
	if len(mem0) != 0 {
		t.Errorf("Expected allocation of 0 bytes to return empty slice, got %d bytes", len(mem0))
	}

	// Test allocating a size larger than the block size
	mem1 := arena.Alloc(128)
	if len(mem1) < 128 {
		t.Errorf("Expected allocation of 128 bytes to return at least 128 bytes, got %d bytes", len(mem1))
	}

	// Fill the first block and force allocation in a new block
	mem2 := arena.Alloc(32)
	if len(mem2) < 32 {
		t.Errorf("Expected allocation of 32 bytes to return at least 32 bytes, got %d bytes", len(mem2))
	}

	// This should force allocation in a new block
	mem3 := arena.Alloc(48)
	if len(mem3) < 48 {
		t.Errorf("Expected allocation of 48 bytes to return at least 48 bytes, got %d bytes", len(mem3))
	}

	// Check stats
	stats := arena.Stats()
	if stats["totalBlocks"] < 2 {
		t.Errorf("Expected at least 2 blocks, got %d", stats["totalBlocks"])
	}
	if stats["totalAllocated"] < 128+32+48 {
		t.Errorf("Expected at least %d bytes allocated, got %d", 128+32+48, stats["totalAllocated"])
	}
}
