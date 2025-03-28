package memory

import (
	"bytes"
	"testing"
)

// TestBufferPoolBasic tests basic buffer pool functionality
func TestBufferPoolBasic(t *testing.T) {
	// Create a pool
	pool := NewBufferPool()

	// Get a buffer from the pool
	buf := pool.Get(64)
	if buf == nil {
		t.Fatal("Expected non-nil buffer from pool")
	}

	// Use the buffer
	data := []byte("Hello, World!")
	*buf = append(*buf, data...)

	// Verify the buffer
	if !bytes.Equal(*buf, data) {
		t.Errorf("Buffer content mismatch: expected %q, got %q", data, *buf)
	}

	// Return the buffer to the pool
	pool.Put(buf)

	// Get another buffer from the pool (should be reset)
	buf2 := pool.Get(64)
	if buf2 == nil {
		t.Fatal("Expected non-nil buffer from pool")
	}

	// Verify the buffer was reset
	if len(*buf2) != 0 {
		t.Errorf("Expected empty buffer, got length %d", len(*buf2))
	}

	// Check stats
	stats := pool.GetStats()
	if stats["gets"] != 2 {
		t.Errorf("Expected 2 gets, got %d", stats["gets"])
	}
	if stats["puts"] != 1 {
		t.Errorf("Expected 1 put, got %d", stats["puts"])
	}
	if stats["tinyGets"] != 2 {
		t.Errorf("Expected 2 tiny gets, got %d", stats["tinyGets"])
	}
}

// TestBufferPoolSizeClasses tests buffer pool size classes
func TestBufferPoolSizeClasses(t *testing.T) {
	// Create a pool
	pool := NewBufferPool()

	// Test different size classes
	sizeClasses := []struct {
		name string
		size int
		stat string
	}{
		{"tiny", 64, "tinyGets"},
		{"small", 256, "smallGets"},
		{"medium", 1024, "mediumGets"},
		{"large", 4096, "largeGets"},
		{"huge", 16384, "hugeGets"},
		{"oversized", 32768, "oversized"},
	}

	for _, sc := range sizeClasses {
		t.Run(sc.name, func(t *testing.T) {
			// Get a buffer of the specified size
			buf := pool.Get(sc.size)
			if buf == nil {
				t.Fatal("Expected non-nil buffer from pool")
			}

			// Verify the buffer capacity
			if cap(*buf) < sc.size {
				t.Errorf("Expected capacity >= %d, got %d", sc.size, cap(*buf))
			}

			// Return the buffer to the pool
			pool.Put(buf)

			// Check stats
			stats := pool.GetStats()
			if stats[sc.stat] != 1 {
				t.Errorf("Expected 1 %s get, got %d", sc.name, stats[sc.stat])
			}
		})
	}
}

// TestBufferPoolReuse tests buffer reuse
func TestBufferPoolReuse(t *testing.T) {
	// Create a pool
	pool := NewBufferPool()

	// Get a buffer from the pool
	buf1 := pool.Get(64)
	if buf1 == nil {
		t.Fatal("Expected non-nil buffer from pool")
	}

	// Use the buffer
	*buf1 = append(*buf1, []byte("test data")...)

	// Return the buffer to the pool
	pool.Put(buf1)

	// Get another buffer from the pool
	buf2 := pool.Get(64)
	if buf2 == nil {
		t.Fatal("Expected non-nil buffer from pool")
	}

	// Verify the buffer was reset
	if len(*buf2) != 0 {
		t.Errorf("Expected empty buffer, got length %d", len(*buf2))
	}

	// Verify it's the same buffer instance (reused)
	// Note: This is implementation-dependent and may not always be true
	// depending on how sync.Pool works internally
	if buf1 != buf2 {
		t.Log("Buffer was not reused (this is not necessarily an error)")
	}
}

// TestGetBuffer tests the global GetBuffer function
func TestGetBuffer(t *testing.T) {
	// Get a buffer from the global pool
	buf := GetBuffer(128)
	if buf == nil {
		t.Fatal("Expected non-nil buffer from global pool")
	}

	// Use the buffer
	*buf = append(*buf, []byte("global buffer test")...)

	// Verify the buffer
	expected := []byte("global buffer test")
	if !bytes.Equal(*buf, expected) {
		t.Errorf("Buffer content mismatch: expected %q, got %q", expected, *buf)
	}

	// Return the buffer to the pool
	PutBuffer(buf)
}

// TestWithBuffer tests the WithBuffer helper function
func TestWithBuffer(t *testing.T) {
	// Use WithBuffer to execute a function with a pooled buffer
	err := WithBuffer(128, func(buf *[]byte) error {
		// Use the buffer
		*buf = append(*buf, []byte("with buffer test")...)

		// Verify the buffer
		expected := []byte("with buffer test")
		if !bytes.Equal(*buf, expected) {
			t.Errorf("Buffer content mismatch: expected %q, got %q", expected, *buf)
		}

		return nil
	})

	if err != nil {
		t.Errorf("WithBuffer returned error: %v", err)
	}
}

// TestAppendBuffer tests the AppendBuffer helper function
func TestAppendBuffer(t *testing.T) {
	// Get a buffer from the pool
	buf := GetBuffer(10)
	if buf == nil {
		t.Fatal("Expected non-nil buffer from pool")
	}

	// Append data that fits in the buffer
	data1 := []byte("small")
	AppendBuffer(buf, data1)

	// Verify the buffer
	if !bytes.Equal(*buf, data1) {
		t.Errorf("Buffer content mismatch: expected %q, got %q", data1, *buf)
	}

	// Append data that exceeds the buffer capacity
	data2 := []byte(" data that exceeds the buffer capacity")
	AppendBuffer(buf, data2)

	// Verify the buffer
	expected := append(data1, data2...)
	if !bytes.Equal(*buf, expected) {
		t.Errorf("Buffer content mismatch: expected %q, got %q", expected, *buf)
	}

	// Return the buffer to the pool
	PutBuffer(buf)
}
