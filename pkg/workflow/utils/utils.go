// Package utils provides utility functions for the workflow package.
// This is a simplified public API for the internal utils system.
package utils

import (
	internalutils "github.com/ppcavalcante/flow-orchestrator/internal/workflow/utils"
)

// SecureRandomFloat64 returns a cryptographically secure random float64 in the range [0.0, 1.0).
// It delegates to the internal implementation which supports test mocking.
func SecureRandomFloat64() float64 {
	return internalutils.SecureRandomFloat64()
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
