package metrics

import (
	"testing"
)

func TestFormatDuration(t *testing.T) {
	testCases := []struct {
		ns       int64
		expected string
	}{
		{500, "500 ns"},
		{1500, "1.50 µs"},
		{1500000, "1.50 ms"},
		{1500000000, "1.50 s"},
	}

	for _, tc := range testCases {
		result := FormatDuration(tc.ns)
		if result != tc.expected {
			t.Errorf("FormatDuration(%d) = %s, expected %s", tc.ns, result, tc.expected)
		}
	}
}
