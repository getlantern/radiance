package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	C "github.com/getlantern/common"
	"github.com/sagernet/sing-box/option"
	singjson "github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/peer"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/unbounded"
	"github.com/getlantern/radiance/vpn"
)

func TestApplyCurrentConfigLoadsCachedServers(t *testing.T) {
	t.Setenv("RADIANCE_COUNTRY", "US")
	dataDir := t.TempDir()
	cfg := cachedConfig()
	buf, err := singjson.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, internal.ConfigFileName), buf, 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := config.NewConfigHandler(ctx, config.Options{
		DataPath: dataDir,
		Logger:   log.NoOpLogger(),
	})
	srvMgr, err := servers.NewManager(dataDir, log.NoOpLogger())
	require.NoError(t, err)
	r := &LocalBackend{
		ctx:         ctx,
		confHandler: ch,
		srvManager:  srvMgr,
		vpnClient:   vpn.NewVPNClient(dataDir, log.NoOpLogger(), nil),
	}

	r.applyCurrentConfig()

	server, found := r.GetServerByTag("cached-out")
	require.True(t, found)
	assert.True(t, server.IsLantern)
	assert.Equal(t, "shadowsocks", server.Type)
	assert.Equal(t, "Shanghai", server.Location.City)
	assert.Equal(t, "CN", server.Location.CountryCode)
}

func cachedConfig() *config.Config {
	return &config.Config{
		Country: "CN",
		OutboundLocations: C.OutboundLocations{
			"cached-out": {
				Country:     "China",
				City:        "Shanghai",
				CountryCode: "CN",
			},
		},
		Options: option.Options{
			Outbounds: []option.Outbound{{
				Tag:  "cached-out",
				Type: "shadowsocks",
				Options: &option.ShadowsocksOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "127.0.0.1",
						ServerPort: 443,
					},
					Method:   "chacha20-ietf-poly1305",
					Password: "password",
				},
			}},
		},
	}
}

// TestNewLocalBackendToleratesInvalidOnDiskState guards the init hardening:
// invalid-but-readable on-disk state must not make NewLocalBackend fatal, so a
// user can always report an issue. Every fixture is readable, so a returned
// error can only come from parsing — a non-IO failure that must stay non-fatal.
func TestNewLocalBackendToleratesInvalidOnDiskState(t *testing.T) {
	dataDir := t.TempDir()
	logDir := t.TempDir()
	// setupDirectories honors these env vars over the Options paths; pin them so
	// the resolved data dir is exactly where the invalid files are staged.
	t.Setenv("RADIANCE_DATA_PATH", dataDir)
	t.Setenv("RADIANCE_LOG_PATH", logDir)

	invalidFiles := map[string][]byte{
		"settings.json":              []byte(`{invalid json}`),
		internal.ConfigFileName:      []byte(`{"options":{"outbounds":[{"type":"future-proto"}]}}`),
		internal.ServersFileName:     []byte(`[{"tag":"bad","type":"future-proto","outbound":{"tag":"bad","type":"future-proto"}}]`),
		internal.SplitTunnelFileName: []byte(`{"version":3,"rules":[{"unknown_field":true}]}`),
	}
	for name, content := range invalidFiles {
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, name), content, 0o600))
	}

	backend, err := NewLocalBackend(context.Background(), Options{DataDir: dataDir, LogDir: logDir, LogLevel: "error"})
	require.NoError(t, err, "invalid but readable on-disk state must not make initialization fatal")
	require.NotNil(t, backend)
	t.Cleanup(backend.Close)
}

func TestExhaustionGate_AllowRateLimitsBelowGap(t *testing.T) {
	prev := defaultExhaustionRefetchGap
	defaultExhaustionRefetchGap = 50 * time.Millisecond
	t.Cleanup(func() { defaultExhaustionRefetchGap = prev })

	var g exhaustionGate
	require.True(t, g.allow(), "first allow must pass on a zero gate")
	assert.False(t, g.allow(), "second allow inside the gap must be rate-limited")
	assert.False(t, g.allow(), "third allow inside the gap must still be rate-limited")

	time.Sleep(defaultExhaustionRefetchGap + 10*time.Millisecond)
	assert.True(t, g.allow(), "allow after the gap elapses must pass again")
	assert.False(t, g.allow(), "post-recovery allow must re-arm the gate")
}

func newTestServer(tag string, isLantern, hardDemoted bool, updatedAt time.Time) *servers.Server {
	srv := &servers.Server{
		Tag:       tag,
		IsLantern: isLantern,
	}
	if hardDemoted || !updatedAt.IsZero() {
		srv.SelectionHistory = &servers.SelectionHistory{
			HardDemoted: hardDemoted,
			UpdatedAt:   updatedAt,
		}
	}
	return srv
}

func TestLanternServersToEvict(t *testing.T) {
	baseTime := time.Unix(0, 0).UTC()

	tests := []struct {
		name     string
		existing []*servers.Server
		incoming int
		limit    int
		want     []string
	}{
		{
			name: "evicts only hard-demoted lantern servers",
			existing: []*servers.Server{
				newTestServer("demoted", true, true, baseTime),
				newTestServer("working", true, false, baseTime),
				newTestServer("users-demoted", false, true, baseTime),
			},
			limit: 60,
			want:  []string{"demoted"},
		},
		{
			name: "hard-demoted lantern server is evicted regardless of incoming config",
			existing: []*servers.Server{
				newTestServer("demoted", true, true, baseTime),
			},
			incoming: 1,
			limit:    60,
			want:     []string{"demoted"},
		},
		{
			name: "under the limit nothing is evicted",
			existing: []*servers.Server{
				newTestServer("a", true, false, baseTime),
				newTestServer("b", true, false, baseTime),
			},
			incoming: 2,
			limit:    60,
		},
		{
			name: "over the limit evicts oldest working servers and keeps the newest",
			existing: []*servers.Server{
				newTestServer("old", true, false, baseTime.Add(1*time.Hour)),
				newTestServer("mid", true, false, baseTime.Add(2*time.Hour)),
				newTestServer("new", true, false, baseTime.Add(3*time.Hour)),
			},
			incoming: 1,
			limit:    3,
			want:     []string{"old"},
		},
		{
			name: "incoming at the limit evicts all existing working servers",
			existing: []*servers.Server{
				newTestServer("a", true, false, baseTime.Add(1*time.Hour)),
				newTestServer("b", true, false, baseTime.Add(2*time.Hour)),
			},
			incoming: 3,
			limit:    3,
			want:     []string{"a", "b"},
		},
		{
			name: "server with no selection history sorts oldest",
			existing: []*servers.Server{
				newTestServer("no-history", true, false, time.Time{}),
				newTestServer("probed", true, false, baseTime.Add(5*time.Hour)),
			},
			incoming: 1,
			limit:    2,
			want:     []string{"no-history"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got := lanternServersToEvict(tt.existing, tt.incoming, tt.limit)
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestAppendManagedServerOptionsIncludesRetainedLanternServers(t *testing.T) {
	options := option.Options{
		Outbounds: []option.Outbound{
			{Tag: "current", Type: "shadowsocks"},
			{Tag: "retained-current", Type: "shadowsocks"},
		},
		Endpoints: []option.Endpoint{
			{Tag: "current-endpoint", Type: "wireguard"},
		},
	}
	managed := []*servers.Server{
		{
			Tag:       "retained-current",
			Type:      "hysteria2",
			IsLantern: true,
			Options:   option.Outbound{Tag: "retained-current", Type: "hysteria2"},
		},
		{
			Tag:       "server-alias-for-current",
			Type:      "hysteria2",
			IsLantern: true,
			Options:   option.Outbound{Tag: "current", Type: "hysteria2"},
		},
		{
			Tag:       "retained-missing",
			Type:      "shadowsocks",
			IsLantern: true,
			Options:   option.Outbound{Tag: "retained-missing", Type: "shadowsocks"},
		},
		{
			Tag:       "user-missing",
			Type:      "trojan",
			IsLantern: false,
			Options:   option.Outbound{Tag: "user-missing", Type: "trojan"},
		},
		{
			Tag:       "missing-option-tag",
			Type:      "shadowsocks",
			IsLantern: true,
			Options:   option.Outbound{Type: "shadowsocks"},
		},
		{
			Tag:       "retained-endpoint",
			Type:      "wireguard",
			IsLantern: true,
			Options:   option.Endpoint{Tag: "retained-endpoint", Type: "wireguard"},
		},
		{
			Tag:       "server-alias-for-current-endpoint",
			Type:      "wireguard",
			IsLantern: true,
			Options:   option.Endpoint{Tag: "current-endpoint", Type: "wireguard"},
		},
		{
			Tag:       "metadata-only",
			IsLantern: true,
		},
	}

	appendManagedServerOptions(&options, managed)

	assert.Equal(t, []string{
		"current",
		"retained-current",
		"retained-missing",
		"user-missing",
	}, outboundTags(options.Outbounds))
	assert.Equal(t, "shadowsocks", options.Outbounds[1].Type, "current config should win duplicate tags")
	assert.Equal(t, []string{
		"current-endpoint",
		"retained-endpoint",
	}, endpointTags(options.Endpoints))
}

func outboundTags(outbounds []option.Outbound) []string {
	tags := make([]string, 0, len(outbounds))
	for _, out := range outbounds {
		tags = append(tags, out.Tag)
	}
	return tags
}

func endpointTags(endpoints []option.Endpoint) []string {
	tags := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		tags = append(tags, endpoint.Tag)
	}
	return tags
}

type fakePeerController struct {
	startCalls atomic.Int64
	stopCalls  atomic.Int64
	startErr   error
	active     atomic.Bool
}

func (f *fakePeerController) Start(_ context.Context) error {
	f.startCalls.Add(1)
	if f.startErr != nil {
		return f.startErr
	}
	f.active.Store(true)
	return nil
}

func (f *fakePeerController) Stop(_ context.Context) error {
	f.stopCalls.Add(1)
	f.active.Store(false)
	return nil
}

func (f *fakePeerController) IsActive() bool             { return f.active.Load() }
func (f *fakePeerController) CurrentStatus() peer.Status { return peer.Status{Active: f.active.Load()} }

// newPeerTestBackend wires a minimal LocalBackend with only the fields
// applyPeerShare touches. settings is initialized to a fresh tempdir per test
// so the rollback path doesn't leak across runs.
func newPeerTestBackend(t *testing.T, fake *fakePeerController) *LocalBackend {
	t.Helper()
	require.NoError(t, settings.InitSettings(t.TempDir()))
	t.Cleanup(settings.Reset)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &LocalBackend{ctx: ctx, peerClient: fake}
}

func TestApplyPeerShare_StartsOnEnable(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)

	require.NoError(t, r.applyPeerShare(true))
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.Equal(t, int64(0), fake.stopCalls.Load())
	assert.True(t, fake.IsActive())
}

func TestApplyPeerShare_StopsOnDisable(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)
	fake.active.Store(true)

	require.NoError(t, r.applyPeerShare(false))
	assert.Equal(t, int64(0), fake.startCalls.Load())
	assert.Equal(t, int64(1), fake.stopCalls.Load())
	assert.False(t, fake.IsActive())
}

// On a Start failure we surface the error so the Dart side can roll back
// its UI, AND we flip the persisted setting back to false so the user-visible
// state matches reality on the next read.
func TestApplyPeerShare_StartFailureRollsBackSetting(t *testing.T) {
	fake := &fakePeerController{startErr: errors.New("no upnp")}
	r := newPeerTestBackend(t, fake)

	require.NoError(t, settings.Patch(settings.Settings{settings.PeerShareEnabledKey: true}))
	require.True(t, settings.GetBool(settings.PeerShareEnabledKey))

	err := r.applyPeerShare(true)
	require.Error(t, err)
	assert.ErrorContains(t, err, "no upnp")
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey),
		"setting must roll back to false after a Start failure")
	assert.False(t, fake.IsActive())
}

func TestPeerStatus_Accessor(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)
	fake.active.Store(true)

	got := r.PeerStatus()
	assert.True(t, got.Active)
}

func TestResumePeerShare_NoopWhenSettingOff(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)

	r.resumePeerShareIfEnabled()
	r.peerWG.Wait()
	assert.Equal(t, int64(0), fake.startCalls.Load())
}

func TestResumePeerShare_StartsWhenSettingOn(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)
	require.NoError(t, settings.Patch(settings.Settings{settings.PeerShareEnabledKey: true}))

	r.resumePeerShareIfEnabled()
	r.peerWG.Wait()
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.True(t, fake.IsActive())
}

// Close must wait for an in-flight auto-resume Start before tearing down,
// then call Stop on the active session — otherwise we leave a registered
// route + open box behind on shutdown.
func TestClose_WaitsForResumeAndStopsActivePeer(t *testing.T) {
	startGate := make(chan struct{})
	fake := &slowStartFake{gate: startGate}
	r := newCloseableTestBackend(t, fake)
	require.NoError(t, settings.Patch(settings.Settings{settings.PeerShareEnabledKey: true}))

	r.resumePeerShareIfEnabled()

	closeReturned := make(chan struct{})
	go func() {
		r.Close()
		close(closeReturned)
	}()

	// Close must NOT return while the resume goroutine is still in Start.
	select {
	case <-closeReturned:
		t.Fatal("Close returned before in-flight resume Start completed")
	case <-time.After(50 * time.Millisecond):
	}

	// Release Start. It returns nil → peer becomes active. Close must then
	// observe IsActive() and call Stop.
	close(startGate)
	select {
	case <-closeReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after resume Start unblocked")
	}
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.Equal(t, int64(1), fake.stopCalls.Load())
}

// slowStartFake blocks on gate until the test releases it, simulating a
// long UPnP discovery so we can race Close against Start.
type slowStartFake struct {
	startCalls atomic.Int64
	stopCalls  atomic.Int64
	active     atomic.Bool
	gate       chan struct{}
}

func (f *slowStartFake) Start(ctx context.Context) error {
	f.startCalls.Add(1)
	select {
	case <-f.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	f.active.Store(true)
	return nil
}
func (f *slowStartFake) Stop(_ context.Context) error {
	f.stopCalls.Add(1)
	f.active.Store(false)
	return nil
}
func (f *slowStartFake) IsActive() bool             { return f.active.Load() }
func (f *slowStartFake) CurrentStatus() peer.Status { return peer.Status{Active: f.active.Load()} }

// newCloseableTestBackend mirrors newPeerTestBackend but provides the fields
// Close needs (closeOnce, cancel) so we can exercise the shutdown path
// end-to-end. vpnClient is left nil, which Close tolerates by design.
func newCloseableTestBackend(t *testing.T, fake peerController) *LocalBackend {
	t.Helper()
	require.NoError(t, settings.InitSettings(t.TempDir()))
	t.Cleanup(settings.Reset)
	ctx, cancel := context.WithCancel(context.Background())
	return &LocalBackend{
		ctx:        ctx,
		cancel:     cancel,
		peerClient: fake,
		closeOnce:  sync.Once{},
	}
}

// Verify the PatchSettings dispatch actually routes PeerShareEnabledKey to
// applyPeerShare. A typo on the diff key would silently break the toggle.
func TestPatchSettings_PeerShareDispatches(t *testing.T) {
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)

	require.NoError(t, r.PatchSettings(settings.Settings{settings.PeerShareEnabledKey: true}))
	assert.Equal(t, int64(1), fake.startCalls.Load())
	assert.True(t, fake.IsActive())

	require.NoError(t, r.PatchSettings(settings.Settings{settings.PeerShareEnabledKey: false}))
	assert.Equal(t, int64(1), fake.stopCalls.Load())
	assert.False(t, fake.IsActive())
}

// Verify PatchSettings routes UnboundedKey to unbounded.Apply via the
// SetApplyHookForTest hook. A typo on the diff key or a removal of
// the Apply call would silently leave the Unbounded toggle persisted
// but inert. The hook fires regardless of the Enabled() gate inside
// Apply, so this catches the dispatch even though we don't prime the
// rest of the manager state.
func TestPatchSettings_UnboundedDispatches(t *testing.T) {
	r := newPeerTestBackend(t, &fakePeerController{})

	var applyCalls atomic.Int32
	unbounded.SetApplyHookForTest(func() { applyCalls.Add(1) })
	t.Cleanup(func() { unbounded.SetApplyHookForTest(nil) })

	require.NoError(t, r.PatchSettings(settings.Settings{settings.UnboundedKey: true}))
	assert.Equal(t, int32(1), applyCalls.Load(), "PatchSettings({UnboundedKey: true}) must dispatch to unbounded.Apply")

	require.NoError(t, r.PatchSettings(settings.Settings{settings.UnboundedKey: false}))
	assert.Equal(t, int32(2), applyCalls.Load(), "PatchSettings({UnboundedKey: false}) must dispatch to unbounded.Apply")

	// A PATCH without UnboundedKey must NOT trigger Apply — confirms
	// the diff check is in place rather than always firing.
	require.NoError(t, r.PatchSettings(settings.Settings{settings.PeerShareEnabledKey: false}))
	assert.Equal(t, int32(2), applyCalls.Load(), "PatchSettings without UnboundedKey must not dispatch to unbounded.Apply")
}

// Construction degrades to a nil peerClient rather than failing, so that a
// peer-client problem can never cost the user the ability to report an issue.
// Everything reachable from IPC must tolerate that.

func TestPeerStatus_NilClientReturnsZeroStatus(t *testing.T) {
	r := newPeerTestBackend(t, nil)
	r.peerClient = nil

	var got peer.Status
	require.NotPanics(t, func() { got = r.PeerStatus() },
		"the IPC /peer/status handler calls this on every request")
	assert.Equal(t, peer.Status{}, got)
}

func TestApplyPeerShare_NilClientReportsUnavailableAndRollsBack(t *testing.T) {
	r := newPeerTestBackend(t, nil)
	r.peerClient = nil
	require.NoError(t, settings.Patch(settings.Settings{settings.PeerShareEnabledKey: true}))

	err := r.applyPeerShare(true)

	require.Error(t, err, "enabling with no peer client must report the outage, not panic")
	assert.ErrorContains(t, err, "peer share unavailable")
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey),
		"the setting must roll back so a persisted \"on\" doesn't survive with nothing behind it")
}

// withPeerShareUnsupported forces the platform gate on for one test. The real
// predicate reads common.Platform, a constant, so the branch is unreachable on
// the host that runs these tests.
func withPeerShareUnsupported(t *testing.T) {
	t.Helper()
	prev := peerShareUnsupported
	peerShareUnsupported = func() bool { return true }
	t.Cleanup(func() { peerShareUnsupported = prev })
}

func TestApplyPeerShare_UnsupportedPlatformRefusesAndRollsBack(t *testing.T) {
	withPeerShareUnsupported(t)
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)
	require.NoError(t, settings.Patch(settings.Settings{settings.PeerShareEnabledKey: true}))

	err := r.applyPeerShare(true)

	require.Error(t, err, "enabling on an unsupported platform must report, not silently no-op")
	assert.ErrorContains(t, err, "not supported")
	assert.Zero(t, fake.startCalls.Load(), "the peer must never start on an unsupported platform")
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey),
		"the setting must roll back so a persisted \"on\" doesn't survive every restart")
}

func TestApplyPeerShare_UnsupportedPlatformDisableIsNoop(t *testing.T) {
	withPeerShareUnsupported(t)
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)

	assert.NoError(t, r.applyPeerShare(false), "turning it off where it never ran is not an error")
	assert.Zero(t, fake.stopCalls.Load(), "nothing was started, so nothing needs stopping")
}

// The auto-resume path reaches Start through applyPeerShare, so the gate has to
// hold there too: a setting persisted before the platform was excluded (or
// synced from another device) would otherwise start a peer on every launch.
func TestResumePeerShare_UnsupportedPlatformDoesNotStart(t *testing.T) {
	withPeerShareUnsupported(t)
	fake := &fakePeerController{}
	r := newPeerTestBackend(t, fake)
	require.NoError(t, settings.Patch(settings.Settings{settings.PeerShareEnabledKey: true}))

	r.resumePeerShareIfEnabled()
	r.peerWG.Wait()

	assert.Zero(t, fake.startCalls.Load(), "auto-resume must not start a peer on an unsupported platform")
	assert.False(t, settings.GetBool(settings.PeerShareEnabledKey),
		"the stale setting is cleared on the way through")
}

func TestApplyPeerShare_NilClientDisableIsNoop(t *testing.T) {
	r := newPeerTestBackend(t, nil)
	r.peerClient = nil

	require.NotPanics(t, func() {
		assert.NoError(t, r.applyPeerShare(false),
			"turning it off when it was never on is not an error")
	})
}

func TestConnectVPN_ShedsConcurrentCalls(t *testing.T) {
	r := &LocalBackend{}

	// Simulate an in-flight connect holding the guard. TryLock is the first thing
	// ConnectVPN does, so contended calls return before touching any dependency.
	require.True(t, r.connectMu.TryLock())

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = r.ConnectVPN(context.Background(), vpn.AutoSelectTag)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.ErrorIsf(t, err, ErrConnectInProgress, "call %d should be shed", i)
	}

	// The reject path must not release the guard; only the holder unlocks it.
	r.connectMu.Unlock()
	assert.True(t, r.connectMu.TryLock())
}

func TestBuildIssueReportMetadataFallsBackToSettings(t *testing.T) {
	settings.Reset()
	t.Cleanup(settings.Reset)
	require.NoError(t, settings.InitSettings(t.TempDir()))
	require.NoError(t, settings.Set(settings.CountryCodeKey, "IR"))
	require.NoError(t, settings.Set(settings.DeviceIDKey, "device-123"))
	require.NoError(t, settings.Set(settings.SplitTunnelKey, true))

	meta := (*LocalBackend)(nil).buildIssueReportMetadata()

	assert.Equal(t, "IR", meta.country)
	assert.Equal(t, "device-123", meta.deviceID, "device ID must survive a backendless report")
	assert.True(t, meta.splitTunnelEnabled, "split-tunnel state must survive a backendless report")
	assert.NotNil(t, meta.reporter, "a reporter is always needed to upload the report")
}
