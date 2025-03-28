package memory

import (
	"sync"
	"sync/atomic"
)

// BufferPool provides a pool of reusable byte buffers with different size classes
// to reduce allocations and memory fragmentation.
type BufferPool struct {
	// Pools for different size classes
	tinyPool   sync.Pool // 64 bytes
	smallPool  sync.Pool // 256 bytes
	mediumPool sync.Pool // 1KB
	largePool  sync.Pool // 4KB
	hugePool   sync.Pool // 16KB

	// Statistics for monitoring
	stats struct {
		gets       int64
		puts       int64
		misses     int64
		tinyGets   int64
		smallGets  int64
		mediumGets int64
		largeGets  int64
		hugeGets   int64
		oversized  int64 // Buffers larger than 16KB
	}
}

// NewBufferPool creates a new buffer pool
func NewBufferPool() *BufferPool {
	return &BufferPool{
		tinyPool: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 64)
				return &buf
			},
		},
		smallPool: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 256)
				return &buf
			},
		},
		mediumPool: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 1024)
				return &buf
			},
		},
		largePool: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 4096)
				return &buf
			},
		},
		hugePool: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, 0, 16384)
				return &buf
			},
		},
	}
}

// Get retrieves a buffer from the appropriate pool based on the requested size
func (p *BufferPool) Get(size int) *[]byte {
	atomic.AddInt64(&p.stats.gets, 1)

	var buf *[]byte

	// Get buffer from appropriate pool based on size
	switch {
	case size <= 64:
		atomic.AddInt64(&p.stats.tinyGets, 1)
		bufVal := p.tinyPool.Get()
		buf = bufVal.(*[]byte) // nolint:errcheck // Safe because we only put *[]byte in the pool
	case size <= 256:
		atomic.AddInt64(&p.stats.smallGets, 1)
		bufVal := p.smallPool.Get()
		buf = bufVal.(*[]byte) // nolint:errcheck // Safe because we only put *[]byte in the pool
	case size <= 1024:
		atomic.AddInt64(&p.stats.mediumGets, 1)
		bufVal := p.mediumPool.Get()
		buf = bufVal.(*[]byte) // nolint:errcheck // Safe because we only put *[]byte in the pool
	case size <= 4096:
		atomic.AddInt64(&p.stats.largeGets, 1)
		bufVal := p.largePool.Get()
		buf = bufVal.(*[]byte) // nolint:errcheck // Safe because we only put *[]byte in the pool
	case size <= 16384:
		atomic.AddInt64(&p.stats.hugeGets, 1)
		bufVal := p.hugePool.Get()
		buf = bufVal.(*[]byte) // nolint:errcheck // Safe because we only put *[]byte in the pool
	default:
		// For very large buffers, don't use the pool to avoid memory bloat
		atomic.AddInt64(&p.stats.oversized, 1)
		newBuf := make([]byte, 0, size)
		buf = &newBuf
	}

	// Reset buffer
	*buf = (*buf)[:0]

	return buf
}

// Put returns a buffer to the appropriate pool based on its capacity
func (p *BufferPool) Put(buf *[]byte) {
	if buf == nil {
		return
	}

	atomic.AddInt64(&p.stats.puts, 1)

	// Return buffer to appropriate pool based on capacity
	capacity := cap(*buf)

	switch {
	case capacity <= 64:
		p.tinyPool.Put(buf)
	case capacity <= 256:
		p.smallPool.Put(buf)
	case capacity <= 1024:
		p.mediumPool.Put(buf)
	case capacity <= 4096:
		p.largePool.Put(buf)
	case capacity <= 16384:
		p.hugePool.Put(buf)
	}
	// Buffers larger than 16KB are not pooled to avoid memory bloat
}

// GetStats returns statistics about the pool's usage
func (p *BufferPool) GetStats() map[string]int64 {
	return map[string]int64{
		"gets":       atomic.LoadInt64(&p.stats.gets),
		"puts":       atomic.LoadInt64(&p.stats.puts),
		"tinyGets":   atomic.LoadInt64(&p.stats.tinyGets),
		"smallGets":  atomic.LoadInt64(&p.stats.smallGets),
		"mediumGets": atomic.LoadInt64(&p.stats.mediumGets),
		"largeGets":  atomic.LoadInt64(&p.stats.largeGets),
		"hugeGets":   atomic.LoadInt64(&p.stats.hugeGets),
		"oversized":  atomic.LoadInt64(&p.stats.oversized),
	}
}

// Global buffer pool for package-wide use
var globalBufferPool = NewBufferPool()

// GetBuffer gets a buffer from the global pool
func GetBuffer(size int) *[]byte {
	return globalBufferPool.Get(size)
}

// PutBuffer returns a buffer to the global pool
func PutBuffer(buf *[]byte) {
	globalBufferPool.Put(buf)
}

// WithBuffer executes a function with a buffer from the pool and returns it afterward
func WithBuffer(size int, fn func(*[]byte) error) error {
	buf := GetBuffer(size)
	defer PutBuffer(buf)
	return fn(buf)
}

// AppendBuffer appends data to a buffer, growing it if necessary
func AppendBuffer(buf *[]byte, data []byte) {
	// Check if we need to grow the buffer
	if cap(*buf)-len(*buf) < len(data) {
		// Create a new buffer with sufficient capacity
		newSize := len(*buf) + len(data)
		newBuf := GetBuffer(newSize)

		// Copy existing data
		*newBuf = append(*newBuf, *buf...)

		// Return old buffer to pool
		PutBuffer(buf)

		// Use new buffer
		buf = newBuf
	}

	// Append data
	*buf = append(*buf, data...)
}
