package peer

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	lbO "github.com/getlantern/lantern-box/option"
)

// testBridgeConfig passes validateAbuseRules with inline rule sets, so a box
// built from it starts without downloading any.
const testBridgeConfig = `{
	"inbounds": [{"type": "mixed", "tag": "stray", "listen": "127.0.0.1", "listen_port": 0}],
	"endpoints": [{"type": "wireguard", "tag": "stray-endpoint", "listen_port": 0}],
	"services": [{"type": "resolved", "tag": "stray-service", "listen": "127.0.0.1", "listen_port": 0}],
	"outbounds": [{"type": "direct", "tag": "direct"}],
	"route": {
		"final": "direct",
		"rule_set": [
			{"type": "inline", "tag": "geosite-malware", "rules": [{"domain": ["malware.invalid"]}]},
			{"type": "inline", "tag": "geoip-malware", "rules": [{"ip_cidr": ["192.0.2.1/32"]}]},
			{"type": "inline", "tag": "geosite-phishing", "rules": [{"domain": ["phishing.invalid"]}]},
			{"type": "inline", "tag": "geosite-cryptominers", "rules": [{"domain": ["miner.invalid"]}]}
		],
		"rules": [
			{"action": "reject", "rule_set": ["geosite-malware", "geoip-malware", "geosite-phishing", "geosite-cryptominers"]},
			{"action": "reject", "ip_cidr": ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]},
			{"action": "reject", "port": [25, 465, 587]}
		]
	}
}`

func testBridgeOptions(shortID string) *lbO.SamizdatBridgeInboundOptions {
	return &lbO.SamizdatBridgeInboundOptions{
		Addrs:          []string{"127.0.0.1:1"},
		PublicKey:      "0001020304050607080910111213141516171819202122232425262728293031",
		ShortID:        shortID,
		ServerName:     "cover.example",
		CertSPKISHA256: "ff0102030405060708091011121314151617181920212223242526272829ffff",
	}
}

func TestBridgeBoxOptionsBypassTheVPNAndListenNowhere(t *testing.T) {
	opts, err := bridgeBoxOptions(box.Context(context.Background()), testBridgeConfig)
	require.NoError(t, err)
	assert.Empty(t, opts.Inbounds)
	assert.Empty(t, opts.Endpoints)
	assert.Empty(t, opts.Services)
	assert.Nil(t, opts.Experimental)
	require.NotNil(t, opts.Route)
	assert.True(t, opts.Route.AutoDetectInterface)
	assert.Equal(t, "direct", opts.Route.Final)

	_, err = bridgeBoxOptions(box.Context(context.Background()), `{"route":`)
	assert.Error(t, err)
}

// A config without direct mode's abuse rules builds no box, so no bridge can
// reach the helper's LAN or abuse destinations.
func TestBridgeBoxRefusesAConfigWithoutAbuseRules(t *testing.T) {
	for _, cfg := range []string{`{}`, `{"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}`} {
		_, err := NewBridgeBox(context.Background(), cfg, func(string, int64) {})
		assert.Error(t, err)
	}
}

func TestBridgeBoxAddsReplacesAndRemovesBridges(t *testing.T) {
	bb, err := NewBridgeBox(context.Background(), testBridgeConfig, func(string, int64) {})
	require.NoError(t, err)
	require.NoError(t, bb.Start())
	t.Cleanup(func() { _ = bb.Close() })
	inbounds := bb.(*bridgeBox).box.Inbound()
	_, stray := inbounds.Get("stray")
	assert.False(t, stray)

	get := func(tag string) adapter.Inbound {
		in, ok := inbounds.Get(tag)
		if !ok {
			return nil
		}
		return in
	}
	require.NoError(t, bb.SetBridges(map[string]*lbO.SamizdatBridgeInboundOptions{
		"bridge-a": testBridgeOptions("0011223344556677"),
		"bridge-b": testBridgeOptions("8899aabbccddeeff"),
	}))
	a, b := get("bridge-a"), get("bridge-b")
	require.NotNil(t, a)
	require.NotNil(t, b)

	require.NoError(t, bb.SetBridges(map[string]*lbO.SamizdatBridgeInboundOptions{
		"bridge-a": testBridgeOptions("0011223344556677"),
		"bridge-b": testBridgeOptions("8899aabbccddee00"),
	}))
	assert.Same(t, a, get("bridge-a"), "unchanged options keep the bridge")
	assert.NotSame(t, b, get("bridge-b"), "a new short ID replaces the bridge")
	require.NotNil(t, get("bridge-b"))

	require.NoError(t, bb.SetBridges(map[string]*lbO.SamizdatBridgeInboundOptions{
		"bridge-b": testBridgeOptions("8899aabbccddee00"),
	}))
	assert.Nil(t, get("bridge-a"))
	assert.NotNil(t, get("bridge-b"))

	bad := testBridgeOptions("zz")
	err = bb.SetBridges(map[string]*lbO.SamizdatBridgeInboundOptions{"bridge-b": bad})
	assert.Error(t, err)
	assert.NotNil(t, get("bridge-b"), "a failed replacement keeps the bridge")

	require.NoError(t, bb.SetBridges(nil))
	assert.Nil(t, get("bridge-b"))
}

func TestBridgeTrackerCountsBothDirectionsUnderTheInbound(t *testing.T) {
	var (
		mu     sync.Mutex
		counts = map[string]int64{}
	)
	tr := bridgeTracker{count: func(tag string, n int64) {
		mu.Lock()
		defer mu.Unlock()
		counts[tag] += n
	}}
	local, remote := net.Pipe()
	defer remote.Close()
	conn := tr.RoutedConnection(context.Background(), local, adapter.InboundContext{Inbound: "bridge-a"}, nil, nil)
	defer conn.Close()

	go func() { _, _ = remote.Write([]byte("hello")) }()
	buf := make([]byte, 5)
	_, err := io.ReadFull(conn, buf)
	require.NoError(t, err)
	go func() { _, _ = io.ReadFull(remote, make([]byte, 3)) }()
	_, err = conn.Write([]byte("abc"))
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, map[string]int64{"bridge-a": 8}, counts)
}

func TestMinLevelHandlerDropsInfo(t *testing.T) {
	h := minLevelHandler{Handler: slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), min: slog.LevelWarn}
	assert.False(t, h.Enabled(context.Background(), slog.LevelInfo))
	assert.True(t, h.Enabled(context.Background(), slog.LevelError))
	assert.False(t, h.WithAttrs(nil).Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, h.WithGroup("g").Enabled(context.Background(), slog.LevelInfo))
}
