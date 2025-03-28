package utils

import (
	"crypto/rand"
	"encoding/binary"
)

// randRead is a variable that holds the function used to read random bytes.
// This allows us to replace it with a mock function during testing.
var randRead = rand.Read

// SecureRandomFloat64Func is the function type for SecureRandomFloat64
type SecureRandomFloat64Func func() float64

// SecureRandomFloat64 is a variable that holds the function used to generate random float64 values.
// This allows us to replace it with a mock function during testing.
var SecureRandomFloat64 = func() float64 {
	var buf [8]byte
	_, err := randRead(buf[:])
	if err != nil {
		// Fallback to a deterministic value if random fails
		return 0.5
	}

	// Convert to uint64 and then to float64 between 0 and 1
	return float64(binary.BigEndian.Uint64(buf[:])) / float64(^uint64(0))
}
