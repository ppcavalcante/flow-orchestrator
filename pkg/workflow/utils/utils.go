// Package utils provides utility functions for the workflow package.
// This is a simplified public API for the internal utils system.
package utils

import (
	"crypto/rand"
	"encoding/binary"
	"math"
)

// SecureRandomFloat64 returns a cryptographically secure random float64 in the range [0.0, 1.0).
func SecureRandomFloat64() float64 {
	var buf [8]byte
	_, err := rand.Read(buf[:])
	if err != nil {
		// Fall back to a deterministic value if crypto/rand fails
		return 0.5
	}

	// Convert to uint64 and then to float64 in [0, 1)
	val := binary.BigEndian.Uint64(buf[:])
	return float64(val) / float64(math.MaxUint64)
}

// StringIntern is a simple string interning function.
// It returns the input string as-is in this simplified implementation.
func StringIntern(s string) string {
	return s
}

// IsValidIdentifier checks if a string is a valid identifier.
func IsValidIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}

	// First character must be a letter or underscore
	if !isLetter(s[0]) && s[0] != '_' {
		return false
	}

	// Remaining characters must be letters, digits, or underscores
	for i := 1; i < len(s); i++ {
		if !isLetter(s[i]) && !isDigit(s[i]) && s[i] != '_' {
			return false
		}
	}

	return true
}

// isLetter returns true if the byte is a letter (a-z, A-Z).
func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isDigit returns true if the byte is a digit (0-9).
func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}
