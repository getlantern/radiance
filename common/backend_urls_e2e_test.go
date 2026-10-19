//go:build lantern_e2e

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalAPIEndpoints(t *testing.T) {
	t.Setenv("RADIANCE_ENV", "production")
	for _, origin := range []string{"http://127.0.0.1:8080", "http://[::1]:8080"} {
		t.Run(origin, func(t *testing.T) {
			t.Setenv("LANTERN_E2E_API_URL", origin)
			require.Equal(t, origin+"/v1", GetBaseURL())
			require.Equal(t, origin+"/v0/pro-server", GetProServerURL())
		})
	}
}

func TestLocalAPIRejectsInvalidOrigins(t *testing.T) {
	for _, origin := range []string{
		"", "http://localhost:8080", "https://127.0.0.1:8080",
		"http://192.168.1.1:8080", "http://0.0.0.0:8080", "http://[::]:8080",
		"http://api.staging.iantem.io:8080", "http://127.0.0.1",
		"http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1:bad",
		"http://user:password@127.0.0.1:8080", "http://127.0.0.1:8080/",
		"http://127.0.0.1:8080/v1", "http://127.0.0.1:8080?query=value",
		"http://127.0.0.1:8080?", "http://127.0.0.1:8080#fragment", "http://127.0.0.1:8080#",
	} {
		t.Run(origin, func(t *testing.T) {
			t.Setenv("LANTERN_E2E_API_URL", origin)
			require.Panics(t, func() { GetBaseURL() })
			require.Panics(t, func() { GetProServerURL() })
		})
	}
}
