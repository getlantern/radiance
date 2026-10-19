//go:build !lantern_e2e

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBackendURLsIgnoreE2EEnvironment(t *testing.T) {
	t.Setenv("LANTERN_E2E_API_URL", "http://127.0.0.1:8080")
	for _, environment := range []string{"", "prod", "production", "dev", "stage", "staging"} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("RADIANCE_ENV", environment)
			if environment == "stage" || environment == "staging" {
				require.Equal(t, StageBaseURL, GetBaseURL())
				require.Equal(t, StageProServerURL, GetProServerURL())
			} else {
				require.Equal(t, BaseURL, GetBaseURL())
				require.Equal(t, ProServerURL, GetProServerURL())
			}
		})
	}
}
