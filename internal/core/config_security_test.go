package core

import (
	"os"
	"testing"
)

func TestParseEnvFloatSecurity(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		val        string
		defaultVal float64
		expected   float64
	}{
		{
			name:       "valid float",
			key:        "TEST_VALID",
			val:        "123.45",
			defaultVal: 10.0,
			expected:   123.45,
		},
		{
			name:       "invalid string returns default",
			key:        "TEST_INVALID",
			val:        "not-a-float",
			defaultVal: 10.0,
			expected:   10.0,
		},
		{
			name:       "partial invalid returns default",
			key:        "TEST_PARTIAL",
			val:        "123.45abc",
			defaultVal: 10.0,
			expected:   10.0,
		},
		{
			name:       "empty string returns default",
			key:        "TEST_EMPTY",
			val:        "",
			defaultVal: 10.0,
			expected:   10.0,
		},
		{
			name:       "extremely large value (overflow) returns default",
			key:        "TEST_OVERFLOW",
			val:        "1e1000",
			defaultVal: 10.0,
			expected:   10.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.val != "" {
				os.Setenv(tt.key, tt.val)
				defer os.Unsetenv(tt.key)
			} else {
				os.Unsetenv(tt.key)
			}

			got := parseEnvFloat(tt.key, tt.defaultVal)
			if got != tt.expected {
				t.Errorf("parseEnvFloat(%s) = %v, want %v", tt.val, got, tt.expected)
			}
		})
	}
}
