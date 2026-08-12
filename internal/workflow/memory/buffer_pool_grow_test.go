package memory

import (
	"bytes"
	"testing"
)

// TestAppendBuffer_GrowPath exercises AppendBuffer's grow branch: when the data does
// not fit in the buffer's remaining capacity, AppendBuffer must allocate a larger
// buffer, copy the existing contents across, return the old buffer to the pool, and
// return the NEW pointer. The original TestAppendBuffer only ever appended data that
// fit within the 64-byte tiny-pool buffer (5 + 38 bytes < cap 64), so this branch —
// the majority of AppendBuffer — was never covered.
func TestAppendBuffer_GrowPath(t *testing.T) {
	// GetBuffer(10) draws from the tiny pool => cap 64.
	buf := GetBuffer(10)
	if got := cap(*buf); got < 64 {
		t.Fatalf("expected tiny-pool cap >= 64, got %d", got)
	}

	// Seed a few bytes so len > 0 before the grow — this makes the "copy existing
	// data" step (append(*newBuf, *buf...)) meaningful, not a no-op on an empty buffer.
	seed := []byte("seed:")
	buf = AppendBuffer(buf, seed)

	// Append well beyond the remaining capacity (200 bytes > cap 64) to force a grow.
	big := bytes.Repeat([]byte("x"), 200)
	buf = AppendBuffer(buf, big)

	want := append(append([]byte{}, seed...), big...)
	if !bytes.Equal(*buf, want) {
		t.Fatalf("grown buffer content mismatch: got len=%d, want len=%d", len(*buf), len(want))
	}
	if cap(*buf) < len(want) {
		t.Fatalf("grown buffer cap %d < needed %d", cap(*buf), len(want))
	}
	PutBuffer(buf)
}

// TestAppendBuffer_GrowFromEmpty covers the grow path when the source buffer is
// empty (len 0) but too small in capacity for the incoming data — the copy step
// copies nothing, and the result is exactly the appended data.
func TestAppendBuffer_GrowFromEmpty(t *testing.T) {
	buf := GetBuffer(10) // cap 64, len 0
	data := bytes.Repeat([]byte("z"), 500)
	out := AppendBuffer(buf, data)
	if !bytes.Equal(*out, data) {
		t.Fatalf("content mismatch: got len=%d, want len=%d", len(*out), len(data))
	}
	PutBuffer(out)
}

// TestPut_NilAndOversized covers Put's two under-exercised edges: a nil pointer
// (the early return — must not panic and must not record a put) and an oversized
// buffer (capacity > 16KB) which matches no size class and must fall through
// unpooled rather than be returned to a pool.
func TestPut_NilAndOversized(t *testing.T) {
	pool := NewBufferPool()

	// nil pointer: early return, no panic, no put recorded.
	pool.Put(nil)
	if got := pool.GetStats()["puts"]; got != 0 {
		t.Fatalf("Put(nil) must not record a put, got %d", got)
	}

	// Oversized buffer (32KB cap > 16KB max class): Put records the attempt but the
	// switch matches no case, so it falls through without pooling.
	oversized := make([]byte, 0, 32*1024)
	pool.Put(&oversized)
	if got := pool.GetStats()["puts"]; got != 1 {
		t.Fatalf("expected 1 put after oversized Put, got %d", got)
	}

	// A subsequent huge Get must still yield a usable buffer (the oversized one was
	// never pooled, so this exercises the pool's New path, not a bad hand-back).
	b := pool.Get(16384)
	if b == nil {
		t.Fatal("expected non-nil buffer from huge pool")
	}
}
