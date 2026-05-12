package portforward

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewManualForwarder_RejectsZeroPort(t *testing.T) {
	t.Parallel()
	_, err := NewManualForwarder(0)
	assert.Error(t, err)
}

func TestNewManualForwarder_StoresPort(t *testing.T) {
	t.Parallel()
	f, err := NewManualForwarder(5698)
	require.NoError(t, err)
	assert.Equal(t, uint16(5698), f.port)
}

func TestManualForwarder_MapPort_ReturnsConfiguredPort(t *testing.T) {
	t.Parallel()
	f, err := NewManualForwarder(5698)
	require.NoError(t, err)

	// Caller-passed internalPort is intentionally ignored — the manual
	// forwarder already has its port fixed at construction.
	m, err := f.MapPort(context.Background(), 12345, "ignored description")
	require.NoError(t, err)
	assert.Equal(t, uint16(5698), m.ExternalPort,
		"manual forwarder must report the configured port, not the "+
			"random one peer.Client.pickInternalPort happened to pass in")
	assert.Equal(t, uint16(5698), m.InternalPort,
		"1:1 router mapping — internal == external")
	assert.Equal(t, "TCP", m.Protocol)
	assert.Equal(t, "manual", m.Method)
	assert.NotEmpty(t, m.InternalIP, "internal IP should resolve via localIP()")
}

func TestManualForwarder_MapPort_RejectsDoubleMap(t *testing.T) {
	t.Parallel()
	f, _ := NewManualForwarder(5698)
	_, err := f.MapPort(context.Background(), 0, "")
	require.NoError(t, err)
	_, err = f.MapPort(context.Background(), 0, "")
	assert.Error(t, err, "second MapPort on a forwarder with an active mapping should fail")
}

func TestManualForwarder_UnmapPort_AllowsRemap(t *testing.T) {
	t.Parallel()
	f, _ := NewManualForwarder(5698)
	_, err := f.MapPort(context.Background(), 0, "")
	require.NoError(t, err)
	require.NoError(t, f.UnmapPort(context.Background()))
	_, err = f.MapPort(context.Background(), 0, "")
	assert.NoError(t, err)
}

func TestManualForwarder_UnmapPort_NilSafe(t *testing.T) {
	t.Parallel()
	var f *ManualForwarder
	assert.NoError(t, f.UnmapPort(context.Background()),
		"nil receiver UnmapPort should be a no-op (matches *Forwarder behavior)")
}

func TestManualForwarder_StartRenewal_NoOp(t *testing.T) {
	t.Parallel()
	f, _ := NewManualForwarder(5698)
	f.StartRenewal(context.Background()) // must not panic
}

// TestManualForwarder_ExternalIP_ReturnsEmpty pins the contract that
// ExternalIP returns "" with no error so the lantern-cloud peer_handler
// fills the IP from the register call's RemoteAddr. A previous revision
// regressed this to call publicip.Detect, which fails on machines where
// Lantern's own VPN tunnel is up — outbound traffic gets routed through
// the tunnel and the discovery endpoints return the tunnel exit's IP
// (or time out entirely), breaking peer registration silently.
func TestManualForwarder_ExternalIP_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	f, err := NewManualForwarder(5698)
	require.NoError(t, err)
	ip, err := f.ExternalIP(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ip,
		"ManualForwarder.ExternalIP must return \"\" so server uses observed RemoteAddr")
}

func TestParseManualPort(t *testing.T) {
	t.Parallel()

	t.Run("empty returns zero, no error", func(t *testing.T) {
		t.Parallel()
		p, err := ParseManualPort("")
		require.NoError(t, err)
		assert.Equal(t, uint16(0), p)
	})

	t.Run("valid port", func(t *testing.T) {
		t.Parallel()
		p, err := ParseManualPort("5698")
		require.NoError(t, err)
		assert.Equal(t, uint16(5698), p)
	})

	t.Run("non-numeric rejected", func(t *testing.T) {
		t.Parallel()
		_, err := ParseManualPort("not-a-port")
		assert.Error(t, err)
	})

	t.Run("out of range rejected", func(t *testing.T) {
		t.Parallel()
		for _, s := range []string{"0", "-1", "65536", "999999"} {
			_, err := ParseManualPort(s)
			assert.Error(t, err, "expected error for %q", s)
		}
	})
}
