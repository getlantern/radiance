package ipc

import (
	"encoding/json"
	"testing"

	box "github.com/getlantern/lantern-box"
	LO "github.com/getlantern/lantern-box/option"
	O "github.com/sagernet/sing-box/option"
	singjson "github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/servers"
)

// TestSamizdatOptionsRoundTrip verifies that samizdat outbound options
// (specifically public_key) survive JSON serialization/deserialization
// through the IPC path. This was the root cause of the "public_key must
// be 64 hex characters (32 bytes), got len=0" bug — standard encoding/json
// doesn't preserve typed Options on option.Outbound's any interface.
func TestSamizdatOptionsRoundTrip(t *testing.T) {
	const testPubKey = "20ebb18d5fdf9bff27fe32ef9501035d8f0bb8dfb481a0a2363181560e0e8115"
	const testShortID = "3b1a8fc7f1edf914"

	original := servers.Servers{
		"lantern": servers.Options{
			Outbounds: []O.Outbound{
				{
					Type: "samizdat",
					Tag:  "samizdat-out-test-route",
					Options: &LO.SamizdatOutboundOptions{
						ServerOptions: O.ServerOptions{
							Server:     "1.2.3.4",
							ServerPort: 443,
						},
						PublicKey:  testPubKey,
						ShortID:    testShortID,
						ServerName: "example.com",
					},
				},
			},
		},
	}

	// Demonstrate the bug: standard json.Marshal/Unmarshal loses the typed Options
	t.Run("standard_json_loses_public_key", func(t *testing.T) {
		buf, err := json.Marshal(original)
		require.NoError(t, err)

		var decoded servers.Servers
		err = json.Unmarshal(buf, &decoded)
		require.NoError(t, err)

		outbounds := decoded["lantern"].Outbounds
		require.Len(t, outbounds, 1)

		// Standard json deserializes Options as map[string]any, not *SamizdatOutboundOptions
		_, ok := outbounds[0].Options.(*LO.SamizdatOutboundOptions)
		assert.False(t, ok, "standard json should NOT preserve typed Options")
	})

	// Verify the fix: sing-box context-aware JSON preserves typed Options
	t.Run("singbox_json_preserves_public_key", func(t *testing.T) {
		ctx := box.BaseContext()

		buf, err := singjson.MarshalContext(ctx, original)
		require.NoError(t, err)

		// Verify public_key is in the serialized JSON
		assert.Contains(t, string(buf), testPubKey, "serialized JSON should contain public_key")

		decoded, err := singjson.UnmarshalExtendedContext[servers.Servers](ctx, buf)
		require.NoError(t, err)

		outbounds := decoded["lantern"].Outbounds
		require.Len(t, outbounds, 1)

		samOpts, ok := outbounds[0].Options.(*LO.SamizdatOutboundOptions)
		require.True(t, ok, "sing-box json should preserve typed Options")
		assert.Equal(t, testPubKey, samOpts.PublicKey, "public_key should survive round-trip")
		assert.Equal(t, testShortID, samOpts.ShortID, "short_id should survive round-trip")
		assert.Equal(t, "example.com", samOpts.ServerName, "server_name should survive round-trip")
	})
}
