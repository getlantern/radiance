package vpn

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"

	O "github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wrapLikeTunnelStart reproduces the wrap chain a TUN bring-up failure travels
// through, so the tests exercise the real unwrap path rather than a bare error:
// sing-tun -> sing-box tun inbound -> inbound manager -> tunnel.connect ->
// tunnel.start.
func wrapLikeTunnelStart(inner error) error {
	err := E.Cause(inner, "configure tun interface")
	err = E.Cause(err, "start inbound/tun[tun-in]")
	err = fmt.Errorf("starting libbox service: %w", err)
	return fmt.Errorf("connecting tunnel: %w", err)
}

// wintunCollision is the production error from sing-tun tun_windows.go:52 when
// an orphaned devnode holds the derived identity. On Windows the create error is
// syscall.Errno(ERROR_ALREADY_EXISTS), whose Is() reports os.ErrExist; using
// os.ErrExist directly keeps the test platform-independent.
func wintunCollision() error {
	return wrapLikeTunnelStart(E.Errors(
		E.Cause(os.ErrExist, "create adapter"),
		E.Cause(errors.New("Element not found."), "open existing adapter"),
	))
}

func TestIsTUNIdentityCollision(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "orphaned devnode holds the derived identity",
			err:  wintunCollision(),
			want: true,
		},
		{
			// engineering#3854: the adapter is created and then fails to enable,
			// reported as ERROR_SET_NOT_FOUND. Not an existence error, so these
			// hosts must not pay a retry — each attempt there burns wintun's
			// fixed 15s WaitForInterface budget.
			name: "device created but never enabled",
			err: wrapLikeTunnelStart(E.Cause(
				errors.New("The property set specified does not exist on the object."),
				"create adapter",
			)),
			want: false,
		},
		{
			name: "unrelated ErrExist elsewhere in bring-up",
			err:  fmt.Errorf("connecting tunnel: %w", fmt.Errorf("opening cache: %w", os.ErrExist)),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isTUNIdentityCollision(tt.err))
		})
	}
}

// The screenshot in the originating report shows both halves of the composite,
// which is what pins the failure to the create/open pair rather than either alone.
func TestWintunCollisionMessageShape(t *testing.T) {
	msg := wintunCollision().Error()
	assert.Contains(t, msg, "create adapter")
	assert.Contains(t, msg, "open existing adapter")
	assert.True(t, errors.Is(wintunCollision(), os.ErrExist))
}

func TestTunIdentityNameSkipsDefault(t *testing.T) {
	// sing-box names the first adapter tun0; retries must not land back on it.
	for attempt := 1; attempt <= maxTUNIdentityRetries; attempt++ {
		assert.NotEqual(t, "tun0", tunIdentityName(attempt))
	}
	assert.Equal(t, "tun1", tunIdentityName(1))
	assert.Equal(t, "tun2", tunIdentityName(2))
}

// The derivation must stay byte-identical to sing-tun's generateGUIDByDeviceName
// or cleanup targets a devnode that doesn't exist and silently removes nothing.
// Goldens are md5("wintun"+name), which is what sing-tun reinterprets as a GUID.
func TestWintunAdapterGUIDMatchesSingTun(t *testing.T) {
	tests := map[string]string{
		"tun0": "3ec6cc0d225680381e097cc9c46ad7b4",
		"tun1": "a950e228515ba379c81addbd57e6205d",
	}
	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			sum := wintunAdapterGUID(name)
			assert.Equal(t, want, hex.EncodeToString(sum[:]))
		})
	}
}

// Every name a bring-up can land on must be a cleanup candidate, including the
// tun0 that sing-box picks on its own — a failed start never reports its name.
func TestTunIdentityCandidatesCoverEveryAttempt(t *testing.T) {
	candidates := tunIdentityCandidates()
	assert.Contains(t, candidates, "tun0")
	for attempt := 1; attempt <= maxTUNIdentityRetries; attempt++ {
		assert.Contains(t, candidates, tunIdentityName(attempt))
	}
	assert.Len(t, candidates, maxTUNIdentityRetries+1)

	seen := map[string]bool{}
	for _, name := range candidates {
		require.False(t, seen[name], "duplicate candidate %q", name)
		seen[name] = true
	}
}

func TestSetTUNInterfaceName(t *testing.T) {
	opts := O.Options{Inbounds: baseInbounds()}

	require.True(t, setTUNInterfaceName(opts, "tun1"))

	var found bool
	for _, inbound := range opts.Inbounds {
		if inbound.Tag != inboundTag {
			continue
		}
		tunOpts, ok := inbound.Options.(*O.TunInboundOptions)
		require.True(t, ok)
		assert.Equal(t, "tun1", tunOpts.InterfaceName)
		found = true
	}
	require.True(t, found, "expected an inbound tagged %q", inboundTag)
}

func TestSetTUNInterfaceNameWithoutTUNInbound(t *testing.T) {
	opts := O.Options{Inbounds: []O.Inbound{{Type: "mixed", Tag: "other"}}}
	assert.False(t, setTUNInterfaceName(opts, "tun1"))
}
