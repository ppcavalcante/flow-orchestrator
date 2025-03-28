package utils

import (
	"testing"
)

func TestSecureRandomFloat64(t *testing.T) {
	// Test that the function returns a value between 0 and 1
	for i := 0; i < 100; i++ {
		val := SecureRandomFloat64()
		if val < 0 || val > 1 {
			t.Errorf("SecureRandomFloat64() returned %f, which is outside the range [0, 1]", val)
		}
	}
}

// Test with mocked crypto/rand that fails
func TestSecureRandomFloat64WithFailure(t *testing.T) {
	// Save the original rand.Read function
	originalRandRead := randRead

	// Restore the original function when the test completes
	defer func() {
		randRead = originalRandRead
	}()

	// Mock rand.Read to always fail
	randRead = func(b []byte) (n int, err error) {
		return 0, &mockError{}
	}

	// When rand.Read fails, the function should return 0.5
	val := SecureRandomFloat64()
	if val != 0.5 {
		t.Errorf("SecureRandomFloat64() returned %f when rand.Read failed, expected 0.5", val)
	}
}

// Mock error type
type mockError struct{}

func (e *mockError) Error() string {
	return "mock error"
}
