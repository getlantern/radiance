package vpn

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	defer closeTunnel()

	assert.True(t, isOpen(), "connection should be open")
	assert.NotNil(t, tInstance, "tInstance should not be nil")

	time.Sleep(100 * time.Millisecond) // give it a sec to start

	err = libbox.NewStandaloneCommandClient().ServiceClose()
	assert.NoError(t, err, "failed to close service")
	assert.False(t, isOpen(), "connection should be closed after closing lbService")
}
