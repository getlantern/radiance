package vpn

import "testing"

func TestNormalizeLocale(t *testing.T) {
	tests := []struct {
		name     string
		locale   string
		expected string
	}{
		{
			name:     "lowercase with hyphen",
			locale:   "zh-cn",
			expected: "ZHCN",
		},
		{
			name:     "lowercase with underscore",
			locale:   "ru_ru",
			expected: "RURU",
		},
		{
			name:     "mixed case with hyphen",
			locale:   "en-US",
			expected: "ENUS",
		},
		{
			name:     "all uppercase",
			locale:   "FAIR",
			expected: "FAIR",
		},
		{
			name:     "all lowercase",
			locale:   "fair",
			expected: "FAIR",
		},
		{
			name:     "multiple hyphens and underscores",
			locale:   "en-US_test",
			expected: "ENUSTEST",
		},
		{
			name:     "empty string",
			locale:   "",
			expected: "",
		},
		{
			name:     "only hyphens and underscores",
			locale:   "-_-_",
			expected: "",
		},
		{
			name:     "numbers and letters",
			locale:   "abc123",
			expected: "ABC123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeLocale(tt.locale)
			if result != tt.expected {
				t.Errorf("normalizeLocale(%q) = %q, expected %q", tt.locale, result, tt.expected)
			}
		})
	}
}
