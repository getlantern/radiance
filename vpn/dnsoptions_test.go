package vpn

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getlantern/radiance/common/settings"
)

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
			assert.Equalf(t, tt.expected, result, "normalizeLocale(%q) should return %q", tt.locale, tt.expected)
		})
	}
}
func TestLocalDNSIP(t *testing.T) {
	tests := []struct {
		name     string
		locale   string
		expected string
	}{
		{
			name:     "FAIR locale returns AliDNS",
			locale:   "FAIR",
			expected: aliDNS,
		},
		{
			name:     "fair lowercase returns AliDNS",
			locale:   "fair",
			expected: aliDNS,
		},
		{
			name:     "ZHCN locale returns AliDNS",
			locale:   "ZHCN",
			expected: aliDNS,
		},
		{
			name:     "zh-cn with hyphen returns AliDNS",
			locale:   "zh-cn",
			expected: aliDNS,
		},
		{
			name:     "zh_cn with underscore returns AliDNS",
			locale:   "zh_cn",
			expected: aliDNS,
		},
		{
			name:     "RURU locale returns AliDNS",
			locale:   "RURU",
			expected: yandexDNS,
		},
		{
			name:     "ru-ru with hyphen returns AliDNS",
			locale:   "ru-ru",
			expected: yandexDNS,
		},
		{
			name:     "en-US returns Quad9",
			locale:   "en-US",
			expected: quad9DNS,
		},
		{
			name:     "enus returns Quad9",
			locale:   "enus",
			expected: quad9DNS,
		},
		{
			name:     "empty locale returns Quad9",
			locale:   "",
			expected: quad9DNS,
		},
		{
			name:     "unknown locale returns Quad9",
			locale:   "fr-FR",
			expected: quad9DNS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup: Set the locale in settings
			t.Cleanup(settings.Reset)
			settings.Set(settings.LocaleKey, tt.locale)

			result := localDNSIP()
			assert.Equalf(t, tt.expected, result, "localDNSIP() with locale %q should return %q", tt.locale, tt.expected)
		})
	}
}
