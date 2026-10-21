package peer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBaseShortID = "0011223344556677"

// launchCfgWithShortID is minimalValidLaunchCfg with the base short ID a real
// launch config carries.
func launchCfgWithShortID() string {
	return strings.Replace(minimalValidLaunchCfg,
		`{"type":"samizdat","tag":"samizdat-in"}`,
		`{"type":"samizdat","tag":"samizdat-in","short_ids":["`+testBaseShortID+`"]}`, 1)
}

// pairingState is the client's stored pairing set and whether one is known.
func pairingState(c *Client) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.pairingShortIDs...), c.pairingKnown
}

func TestMergeShortIDs(t *testing.T) {
	assert.Equal(t,
		[]string{testBaseShortID, "1111111111111111", "aaaaaaaaaaaaaaaa"},
		mergeShortIDs([]string{testBaseShortID},
			[]string{"1111111111111111", "bad", "1111111111111111", testBaseShortID, "AAAAAAAAAAAAAAAA", "zzzzzzzzzzzzzzzz"}),
		"base first; invalid, duplicate and base-equal pairing IDs dropped; hex lowercased")
	assert.Equal(t, []string{testBaseShortID}, mergeShortIDs([]string{testBaseShortID}, nil))
}

func TestInboundShortIDs(t *testing.T) {
	assert.Equal(t, []string{testBaseShortID}, inboundShortIDs(launchCfgWithShortID()))
	assert.Empty(t, inboundShortIDs(minimalValidLaunchCfg))
	assert.Empty(t, inboundShortIDs("not json"))
}

func TestClient_StartAddsRegisterPairingShortIDs(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{"1111111111111111"}
	srv.registerResp.PairingShortIDsKnown = true
	b := &fakeBoxService{}
	c := newTestClient(t, &fakeForwarder{}, b, srv, func(cfg *Config) { cfg.HeartbeatInterval = time.Hour })
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	assert.Equal(t, []string{testBaseShortID, "1111111111111111"}, b.lastShortIDs())
	assert.Equal(t, []string{testBaseShortID, "1111111111111111"}, b.shortIDsAtStart(),
		"the box must hold the pairing IDs before it starts serving")
}

func TestClient_StartIgnoresUnknownRegisterPairingShortIDs(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{"1111111111111111"}
	b := &fakeBoxService{}
	c := newTestClient(t, &fakeForwarder{}, b, srv, func(cfg *Config) { cfg.HeartbeatInterval = time.Hour })
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	assert.Nil(t, b.lastShortIDs(), "a set the server does not vouch for is not applied")
}

func TestClient_WithoutBaseShortIDLeavesInboundAlone(t *testing.T) {
	srv := newStubServer(t) // minimalValidLaunchCfg has no short_ids
	srv.registerResp.PairingShortIDs = []string{"1111111111111111"}
	srv.registerResp.PairingShortIDsKnown = true
	b := &fakeBoxService{}
	c := newTestClient(t, &fakeForwarder{}, b, srv, func(cfg *Config) { cfg.HeartbeatInterval = time.Hour })
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	assert.Nil(t, b.lastShortIDs(), "never replace the set without the base ID the verifier uses")
}

func TestClient_HeartbeatReplacesPairingShortIDs(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.heartbeatShortIDs.Store(&[]string{"2222222222222222"})
	b := &fakeBoxService{}
	c := newTestClient(t, &fakeForwarder{}, b, srv, func(cfg *Config) { cfg.HeartbeatInterval = 20 * time.Millisecond })
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	require.Eventually(t, func() bool {
		return slices.Equal(b.lastShortIDs(), []string{testBaseShortID, "2222222222222222"})
	}, 2*time.Second, 10*time.Millisecond)

	// An ended pairing drops out at the next heartbeat; the base ID stays.
	srv.heartbeatShortIDs.Store(&[]string{})
	require.Eventually(t, func() bool {
		return slices.Equal(b.lastShortIDs(), []string{testBaseShortID})
	}, 2*time.Second, 10*time.Millisecond)
}

func TestClient_KnownEmptyHeartbeatRevokesPairingShortIDs(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{"1111111111111111"}
	srv.registerResp.PairingShortIDsKnown = true
	srv.heartbeatShortIDs.Store(&[]string{})
	b := &fakeBoxService{}
	c := newTestClient(t, &fakeForwarder{}, b, srv, func(cfg *Config) { cfg.HeartbeatInterval = 20 * time.Millisecond })
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	require.Eventually(t, func() bool {
		return slices.Equal(b.lastShortIDs(), []string{testBaseShortID})
	}, 2*time.Second, 10*time.Millisecond, "an authoritative empty set means no pairings")
	ids, known := pairingState(c)
	assert.True(t, known)
	assert.Empty(t, ids)
}

func TestClient_UnknownHeartbeatKeepsPairingShortIDs(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.heartbeatShortIDs.Store(&[]string{"2222222222222222"})
	b := &fakeBoxService{}
	c := newTestClient(t, &fakeForwarder{}, b, srv, func(cfg *Config) { cfg.HeartbeatInterval = 20 * time.Millisecond })
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	require.Eventually(t, func() bool {
		return slices.Equal(b.lastShortIDs(), []string{testBaseShortID, "2222222222222222"})
	}, 2*time.Second, 10*time.Millisecond)

	// The server now answers without the fields, as one without the ticket
	// key or with an unreadable body does.
	srv.heartbeatShortIDs.Store(nil)
	updates := len(b.shortIDUpdates())
	after := srv.heartbeatCount.Load()
	require.Eventually(t, func() bool { return srv.heartbeatCount.Load() >= after+3 }, 2*time.Second, 10*time.Millisecond)

	got := b.shortIDUpdates()
	for _, set := range got[updates:] {
		assert.Equal(t, []string{testBaseShortID, "2222222222222222"}, set,
			"a heartbeat that does not vouch for a set must never revoke")
	}
	assert.Equal(t, []string{testBaseShortID, "2222222222222222"}, b.lastShortIDs())
	ids, known := pairingState(c)
	assert.True(t, known)
	assert.Equal(t, []string{"2222222222222222"}, ids)
}

func TestClient_FailedHeartbeatKeepsPairingShortIDs(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{"1111111111111111"}
	srv.registerResp.PairingShortIDsKnown = true
	srv.heartbeatStatus = http.StatusInternalServerError
	b := &fakeBoxService{}
	c := newTestClient(t, &fakeForwarder{}, b, srv, func(cfg *Config) { cfg.HeartbeatInterval = 20 * time.Millisecond })
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	require.Eventually(t, func() bool { return srv.heartbeatCount.Load() >= 3 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, [][]string{{testBaseShortID, "1111111111111111"}}, b.shortIDUpdates(),
		"a failed heartbeat says nothing about pairings, so the last set stays")
}

func TestClient_HeartbeatTransportErrorKeepsPairingShortIDs(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{"1111111111111111"}
	srv.registerResp.PairingShortIDsKnown = true
	srv.heartbeatShortIDs.Store(&[]string{})
	// Each heartbeat outlives its timeout, so the client sees a transport
	// error, never the authoritative empty set the server would send.
	srv.onHeartbeat = func() { time.Sleep(100 * time.Millisecond) }
	b := &fakeBoxService{}
	c := newTestClient(t, &fakeForwarder{}, b, srv, func(cfg *Config) {
		cfg.HeartbeatInterval = 20 * time.Millisecond
		cfg.HeartbeatTimeout = 5 * time.Millisecond
	})
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	require.Eventually(t, func() bool { return srv.heartbeatCount.Load() >= 3 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, [][]string{{testBaseShortID, "1111111111111111"}}, b.shortIDUpdates())
}

func TestClient_FailedShortIDUpdateKeepsPreviousSet(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{"1111111111111111"}
	srv.registerResp.PairingShortIDsKnown = true
	b := &fakeBoxService{}
	c := newTestClient(t, &fakeForwarder{}, b, srv, func(cfg *Config) { cfg.HeartbeatInterval = 20 * time.Millisecond })
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	b.setUpdateErr(errors.New("refused"))
	srv.heartbeatShortIDs.Store(&[]string{"2222222222222222"})
	after := srv.heartbeatCount.Load()
	require.Eventually(t, func() bool { return srv.heartbeatCount.Load() >= after+3 }, 2*time.Second, 10*time.Millisecond)
	ids, known := pairingState(c)
	assert.True(t, known)
	assert.Equal(t, []string{"1111111111111111"}, ids, "a set the inbound refused is not recorded as applied")
}

// rotatedBoxHas reports whether any box built by a rotation, not by Start,
// was already accepting want when it started.
func rotatedBoxHas(port *portBoxes, want []string) bool {
	port.mu.Lock()
	defer port.mu.Unlock()
	for _, b := range port.boxes[min(1, len(port.boxes)):] {
		if b.started.Load() && slices.Equal(b.shortIDsAtStart(), want) {
			return true
		}
	}
	return false
}

func TestClient_FailedPushIsRetriedOnUnknownHeartbeat(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{"1111111111111111"}
	srv.registerResp.PairingShortIDsKnown = true
	// Heartbeats answer without the fields, so none vouches for a set.
	b := &fakeBoxService{updateErr: errors.New("refused")}
	c := newTestClient(t, &fakeForwarder{}, b, srv, func(cfg *Config) { cfg.HeartbeatInterval = 20 * time.Millisecond })
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	assert.Nil(t, b.lastShortIDs(), "the push at Start failed")

	b.setUpdateErr(nil)
	require.Eventually(t, func() bool {
		return slices.Equal(b.lastShortIDs(), []string{testBaseShortID, "1111111111111111"})
	}, 2*time.Second, 10*time.Millisecond, "a heartbeat pushes the recorded set the box does not hold")
	after := srv.heartbeatCount.Load()
	require.Eventually(t, func() bool { return srv.heartbeatCount.Load() >= after+3 }, 2*time.Second, 10*time.Millisecond)
	assert.Len(t, b.shortIDUpdates(), 1, "once the box holds the set it is not pushed again")
}

func TestClient_RotationReappliesPairingShortIDs(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{"3333333333333333"}
	srv.registerResp.PairingShortIDsKnown = true
	port := &portBoxes{}
	c := newTestClient(t, &fakeForwarder{externalIP: "203.0.113.42"}, &fakeBoxService{}, srv, func(cfg *Config) {
		cfg.CredRotationInterval = 50 * time.Millisecond
		cfg.HeartbeatInterval = time.Hour
		cfg.BuildBoxService = port.build
	})
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	require.Eventually(t, func() bool {
		return rotatedBoxHas(port, []string{testBaseShortID, "3333333333333333"})
	}, 2*time.Second, 10*time.Millisecond, "a rotated box must hold the pairing IDs before it starts")
}

func TestClient_RotationKeepsLastKnownPairingShortIDs(t *testing.T) {
	srv := newStubServer(t)
	var registers atomic.Int64
	srv.registerRespFn = func() RegisterResponse {
		resp := RegisterResponse{
			RouteID:                  "00000000-0000-0000-0000-000000000123",
			ServerConfig:             launchCfgWithShortID(),
			HeartbeatIntervalSeconds: 60,
		}
		if registers.Add(1) == 1 {
			resp.PairingShortIDs = []string{"3333333333333333"}
			resp.PairingShortIDsKnown = true
		}
		return resp
	}
	port := &portBoxes{}
	c := newTestClient(t, &fakeForwarder{externalIP: "203.0.113.42"}, &fakeBoxService{}, srv, func(cfg *Config) {
		cfg.CredRotationInterval = 50 * time.Millisecond
		cfg.HeartbeatInterval = time.Hour
		cfg.BuildBoxService = port.build
	})
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	require.Eventually(t, func() bool {
		return rotatedBoxHas(port, []string{testBaseShortID, "3333333333333333"})
	}, 2*time.Second, 10*time.Millisecond, "a rotation whose register does not vouch for a set keeps the last known one")
}

func TestClient_RotationKeepsNewerHeartbeatSet(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{"3333333333333333", "5555555555555555"}
	srv.registerResp.PairingShortIDsKnown = true
	var (
		c     *Client
		mu    sync.Mutex
		boxes []*fakeBoxService
	)
	c = newTestClient(t, &fakeForwarder{externalIP: "203.0.113.42"}, &fakeBoxService{}, srv, func(cfg *Config) {
		cfg.CredRotationInterval = time.Hour
		cfg.HeartbeatInterval = time.Hour
		cfg.BuildBoxService = func(_ context.Context, options string) (boxService, error) {
			mu.Lock()
			b := &fakeBoxService{gotConfig: options}
			boxes = append(boxes, b)
			n := len(boxes)
			mu.Unlock()
			if n == 2 {
				// A heartbeat lands while the rotation builds its box: the
				// 3333 pairing has ended.
				c.applyPairingShortIDs([]string{"5555555555555555"}, true)
			}
			return b, nil
		}
	})
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	require.NoError(t, c.rotateCreds(context.Background()))

	mu.Lock()
	require.Len(t, boxes, 2)
	rotated := boxes[1]
	mu.Unlock()
	assert.Equal(t, []string{testBaseShortID, "5555555555555555"}, rotated.lastShortIDs(),
		"the rotation's register snapshot must not restore a pairing a newer heartbeat revoked")
	ids, known := pairingState(c)
	assert.True(t, known)
	assert.Equal(t, []string{"5555555555555555"}, ids)
}

// realSamizdatOptions is a launch config the real lantern-box samizdat inbound
// accepts, listening on a free loopback port.
func realSamizdatOptions(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"example.com"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	priv := make([]byte, 32)
	_, err = rand.Read(priv)
	require.NoError(t, err)
	b, err := json.Marshal(map[string]any{"inbounds": []any{map[string]any{
		"type": "samizdat", "tag": "samizdat-in", "listen": "127.0.0.1", "listen_port": 0,
		"private_key":       hex.EncodeToString(priv),
		"short_ids":         []string{testBaseShortID},
		"cert_pem":          string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		"key_pem":           string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		"masquerade_domain": "example.com",
	}}})
	require.NoError(t, err)
	return string(b)
}

// TestSamizdatInbound_RealBox checks the seam the fakes stand in for: a
// sing-box built by defaultBuildBoxService exposes lantern-box's samizdat
// inbound, and its UpdateShortIDs is reachable.
func TestSamizdatInbound_RealBox(t *testing.T) {
	b, err := defaultBuildBoxService(context.Background(), realSamizdatOptions(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	u, ok := samizdatInbound(b)
	require.True(t, ok, "the box's samizdat-in inbound must expose UpdateShortIDs")
	require.NoError(t, u.UpdateShortIDs([]string{testBaseShortID, "4444444444444444"}))
	require.Error(t, u.UpdateShortIDs(nil), "lantern-box's own validation runs: an empty set is refused")
}
