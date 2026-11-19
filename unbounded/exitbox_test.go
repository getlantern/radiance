package unbounded

import (
	"context"
	"testing"

	box "github.com/getlantern/lantern-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/internal/hostaccess"
)

func exitBoxContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(box.BaseContext())
	t.Cleanup(cancel)
	return hostaccess.Context(ctx)
}

func TestNewExitBoxStartsAMinimalBox(t *testing.T) {
	b, err := newExitBox(exitBoxContext(t), `{"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}`)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NotNil(t, b.Router())
	// RunExit checks rule sets through the router, so it must keep sing-box's lookup.
	_, ok := b.Router().(interface {
		RuleSet(tag string) (adapter.RuleSet, bool)
	})
	assert.True(t, ok, "the router must look up rule sets by tag")
	b.ResetNetwork()
}

func TestNewExitBoxRefusesHostAccess(t *testing.T) {
	_, err := newExitBox(exitBoxContext(t), `{"outbounds":[{"type":"tor","tag":"tor"},{"type":"direct","tag":"direct"}]}`)
	require.Error(t, err)
	assert.ErrorIs(t, err, hostaccess.ErrDenied)
}

func TestNewExitBoxRejectsBadJSON(t *testing.T) {
	_, err := newExitBox(exitBoxContext(t), `{`)
	require.Error(t, err)
}

func TestNewExitBoxUsesThePlatformMonitor(t *testing.T) {
	platform := &exitPlatform{}
	ctx, cancel := context.WithCancel(exitContext(platform))
	t.Cleanup(cancel)
	// RunExit forces auto_detect_interface on every exit box.
	b, err := newExitBox(ctx, `{"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct","auto_detect_interface":true}}`)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	assert.Equal(t, int32(1), platform.monitorStarts.Load(), "the box must watch the default interface through the platform")
}
