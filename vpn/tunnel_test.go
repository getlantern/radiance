package vpn

import (
	"path/filepath"
	"testing"

	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/vpn/ipc"
)

func TestEstablishConnection(t *testing.T) {
	tmp := t.TempDir()
	tOpts, _, err := testBoxOptions(tmp)
	require.NoError(t, err, "failed to get test box options")

	tOpts.Route.RuleSet = baseOpts().Route.RuleSet
	tOpts.Route.RuleSet[0].LocalOptions.Path = filepath.Join(tmp, splitTunnelFile)
	tOpts.Route.Rules = append([]option.Rule{baseOpts().Route.Rules[2]}, tOpts.Route.Rules...)
	newSplitTunnel(tmp)

	err = establishConnection("", "", *tOpts, tmp, nil)
	require.NoError(t, err, "failed to establish connection")
	t.Cleanup(func() {
		if tInstance != nil {
			tInstance.close()
		}
	})

	tun := tInstance
	assert.NotNil(t, tun, "tInstance should not be nil")
	assert.Equal(t, ipc.StatusRunning, tun.Status(), "tunnel should be running")

	assert.NoError(t, tun.close(), "failed to close lbService")
	assert.Equal(t, ipc.StatusClosed, tun.Status(), "tun should be closed")
}
