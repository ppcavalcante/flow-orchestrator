package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsValidIdentifier(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"Empty string", "", false},
		{"Valid identifier", "validName", true},
		{"Valid with underscore", "valid_name", true},
		{"Valid with numbers", "name123", true},
		{"Starting with number", "1name", false},
		{"Special characters", "name@123", false},
		{"Underscore only", "_", true},
		{"Starting with underscore", "_name", true},
		{"Space in middle", "invalid name", false},
		{"Hyphen", "invalid-name", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValidIdentifier(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestStringIntern(t *testing.T) {
	// Test interning of strings
	str1 := "test string"
	str2 := "test string"
	str3 := "different string"

	// Same strings should return same pointer
	intern1 := StringIntern(str1)
	intern2 := StringIntern(str2)
	assert.Equal(t, &intern1, &intern2)

	// Different strings should return different pointers
	intern3 := StringIntern(str3)
	assert.NotEqual(t, &intern1, &intern3)

	// Empty string should work
	empty1 := StringIntern("")
	empty2 := StringIntern("")
	assert.Equal(t, &empty1, &empty2)
}

func TestIsLetter(t *testing.T) {
	tests := []struct {
		name     string
		input    byte
		expected bool
	}{
		{"Lowercase a", byte('a'), true},
		{"Lowercase z", byte('z'), true},
		{"Uppercase A", byte('A'), true},
		{"Uppercase Z", byte('Z'), true},
		{"Number", byte('5'), false},
		{"Special character", byte('@'), false},
		{"Space", byte(' '), false},
		{"Underscore", byte('_'), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isLetter(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsDigit(t *testing.T) {
	tests := []struct {
		name     string
		input    byte
		expected bool
	}{
		{"Zero", byte('0'), true},
		{"Nine", byte('9'), true},
		{"Letter", byte('a'), false},
		{"Special character", byte('@'), false},
		{"Space", byte(' '), false},
		{"Underscore", byte('_'), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isDigit(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSecureRandomFloat64(t *testing.T) {
	// Test that values are within expected range
	for i := 0; i < 1000; i++ {
		val := SecureRandomFloat64()
		assert.GreaterOrEqual(t, val, 0.0)
		assert.Less(t, val, 1.0)
	}

	// Test that we get different values
	val1 := SecureRandomFloat64()
	val2 := SecureRandomFloat64()
	assert.NotEqual(t, val1, val2)
}
