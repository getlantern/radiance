//go:build android || ios || (darwin && !standalone)

package ipc

import (
	"testing"

	"github.com/stretchr/testify/require"

	wire "github.com/getlantern/common/usermessage"

	"github.com/getlantern/radiance/backend"
)

func TestFallbackOptionsPreserveUserMessageCapabilities(t *testing.T) {
	capabilities := wire.ClientCapabilities{
		Version:  wire.CapabilityUserMessagesV1,
		Surfaces: []wire.Surface{wire.SurfaceSnackbar},
		Actions:  []wire.ActionType{wire.ActionTypeOpenPlans},
	}
	client := newClient()
	client.opts = cloneBackendOptions(backend.Options{
		UserMessageCapabilities: capabilities,
		EnvOverrides:            map[string]string{"RADIANCE_ENV": "staging"},
	})

	fallback := client.fallbackOptions()
	require.Equal(t, capabilities, fallback.UserMessageCapabilities)
	require.Equal(t, "staging", fallback.EnvOverrides["RADIANCE_ENV"])

	fallback.UserMessageCapabilities.Surfaces[0] = "changed"
	fallback.EnvOverrides["RADIANCE_ENV"] = "changed"
	require.Equal(t, wire.SurfaceSnackbar, client.opts.UserMessageCapabilities.Surfaces[0])
	require.Equal(t, "staging", client.opts.EnvOverrides["RADIANCE_ENV"])
}
