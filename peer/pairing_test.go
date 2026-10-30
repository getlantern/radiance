package peer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/tracker/peerconn"

	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/helper"
	"github.com/getlantern/radiance/internal/testutil"
)

const (
	pairingA = "a1b2c3d4-0000-4000-8000-00000000000a"
	pairingB = "a1b2c3d4-0000-4000-8000-00000000000b"
	sidA1    = "1111111111111111"
	sidA2    = "1111111111112222"
	sidB1    = "2222222222222222"
)

func sid(t *testing.T, s string) [8]byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	require.NoError(t, err)
	require.Len(t, raw, 8)
	return [8]byte(raw)
}

func groupsAB() []PairingShortIDSet {
	return []PairingShortIDSet{
		{PairingID: pairingA, ShortIDs: []string{sidA1, sidA2}},
		{PairingID: pairingB, ShortIDs: []string{sidB1}},
	}
}

func TestAPI_DecodesPairingShortIDSets(t *testing.T) {
	const body = `{"route_id":"r","pairing_short_ids":["1111111111111111","2222222222222222"],` +
		`"pairing_short_ids_known":true,"pairing_short_ids_revision":"1700000000000",` +
		`"pairing_short_id_sets":[{"pairing_id":"` + pairingA + `","short_ids":["1111111111111111"]},` +
		`{"pairing_id":"` + pairingB + `","short_ids":["2222222222222222"]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	api := NewAPI(srv.Client(), srv.URL, "dev")
	want := []PairingShortIDSet{
		{PairingID: pairingA, ShortIDs: []string{sidA1}},
		{PairingID: pairingB, ShortIDs: []string{sidB1}},
	}

	reg, err := api.Register(context.Background(), RegisterRequest{})
	require.NoError(t, err)
	assert.Equal(t, want, reg.PairingShortIDSets)
	assert.Equal(t, Revision(1700000000000), reg.PairingShortIDsRevision)

	hb, err := api.Heartbeat(context.Background(), "r", 0)
	require.NoError(t, err)
	assert.Equal(t, want, hb.PairingShortIDSets)
	assert.True(t, hb.PairingShortIDsKnown)

	// An older server sends no groups.
	var old HeartbeatResponse
	require.NoError(t, json.Unmarshal([]byte(`{"pairing_short_ids":["1111111111111111"],"pairing_short_ids_known":true,"pairing_short_ids_revision":5}`), &old))
	assert.Nil(t, old.PairingShortIDSets)
}

func TestGroupPairingShortIDs(t *testing.T) {
	flat := []string{sidA1, sidA2, sidB1}

	m, ok := groupPairingShortIDs(flat, groupsAB(), []string{testBaseShortID})
	require.True(t, ok)
	assert.Equal(t, map[[8]byte]string{sid(t, sidA1): pairingA, sid(t, sidA2): pairingA, sid(t, sidB1): pairingB}, m)

	upper := []string{strings.ToUpper(sidA1), sidA2, sidB1}
	_, ok = groupPairingShortIDs(upper, groupsAB(), nil)
	assert.True(t, ok, "hex case is ignored")

	for name, f := range map[string][]string{
		"reordered": {sidB1, sidA1, sidA2},
		"shorter":   {sidA1, sidA2},
		"longer":    {sidA1, sidA2, sidB1, "3333333333333333"},
		"different": {sidA1, sidA2, "3333333333333333"},
		"empty":     nil,
	} {
		_, ok := groupPairingShortIDs(f, groupsAB(), nil)
		assert.False(t, ok, name)
	}
	_, ok = groupPairingShortIDs(flat, nil, nil)
	assert.False(t, ok, "no groups")

	// The launch config's own ID never names a pairing, nor does a bad entry
	// or a group without a pairing ID.
	groups := []PairingShortIDSet{
		{PairingID: pairingA, ShortIDs: []string{testBaseShortID, "zz"}},
		{PairingID: "", ShortIDs: []string{sidB1}},
	}
	m, ok = groupPairingShortIDs([]string{testBaseShortID, "zz", sidB1}, groups, []string{testBaseShortID})
	require.True(t, ok)
	assert.Empty(t, m)
}

// pairingClient is a started client whose register answered flat at revision
// 100 with no groups, as an older server replica does.
func pairingClient(t *testing.T, opts ...func(*Config)) *Client {
	t.Helper()
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{sidA1, sidA2, sidB1}
	srv.registerResp.PairingShortIDsKnown = true
	srv.registerResp.PairingShortIDsRevision = 100
	opts = append([]func(*Config){func(cfg *Config) { cfg.HeartbeatInterval = time.Hour }}, opts...)
	c := newTestClient(t, &fakeForwarder{}, &fakeBoxService{}, srv, opts...)
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	return c
}

func TestClient_GroupsAcceptedAtTheAppliedRevisionWhenTheyMatch(t *testing.T) {
	c := pairingClient(t)
	assert.Equal(t, "", c.pairingFor(sid(t, sidA1)), "no groups yet")

	// A newer replica answers with groups at the revision already applied.
	c.offerPairingShortIDs([]string{sidA1, sidA2, sidB1}, groupsAB(), true, 100)
	assert.Equal(t, pairingA, c.pairingFor(sid(t, sidA1)))
	assert.Equal(t, pairingA, c.pairingFor(sid(t, sidA2)))
	assert.Equal(t, pairingB, c.pairingFor(sid(t, sidB1)))

	// And an older replica answering flat at the same revision afterwards
	// leaves them alone.
	c.offerPairingShortIDs([]string{sidA1, sidA2, sidB1}, nil, true, 100)
	assert.Equal(t, pairingB, c.pairingFor(sid(t, sidB1)))
	ids, _ := pairingState(c)
	assert.Equal(t, []string{sidA1, sidA2, sidB1}, ids)
}

func TestClient_GroupsRejectedAtTheAppliedRevisionWhenTheyDiffer(t *testing.T) {
	c := pairingClient(t)
	mismatched := []PairingShortIDSet{
		{PairingID: pairingA, ShortIDs: []string{sidA1}},
		{PairingID: pairingB, ShortIDs: []string{sidB1}},
	}
	// The flat list in the response is ignored at this revision; the groups
	// are checked against the set applied, which they do not describe.
	c.offerPairingShortIDs([]string{sidA1, sidB1}, mismatched, true, 100)
	assert.Equal(t, "", c.pairingFor(sid(t, sidA1)))
	assert.Equal(t, "", c.pairingFor(sid(t, sidB1)))
	ids, _ := pairingState(c)
	assert.Equal(t, []string{sidA1, sidA2, sidB1}, ids, "the flat set is unchanged")
}

func TestClient_GroupsFollowTheStrictlyNewerRuleOtherwise(t *testing.T) {
	c := pairingClient(t)

	// Older than the applied revision: neither the set nor its groups.
	c.offerPairingShortIDs([]string{sidA1, sidA2, sidB1}, groupsAB(), true, 99)
	assert.Equal(t, "", c.pairingFor(sid(t, sidA1)))

	// Newer, with groups that do not match their own set: the set is taken,
	// the groups are not.
	c.offerPairingShortIDs([]string{sidA1, sidB1}, groupsAB(), true, 200)
	ids, _ := pairingState(c)
	assert.Equal(t, []string{sidA1, sidB1}, ids)
	assert.Equal(t, "", c.pairingFor(sid(t, sidA1)))

	// Newer with matching groups: both.
	c.offerPairingShortIDs([]string{sidA1, sidA2, sidB1}, groupsAB(), true, 300)
	assert.Equal(t, pairingA, c.pairingFor(sid(t, sidA2)))

	// Newer without groups: the set is taken, and the newest grouped set's
	// mapping stays.
	c.offerPairingShortIDs([]string{sidB1}, nil, true, 400)
	ids, _ = pairingState(c)
	assert.Equal(t, []string{sidB1}, ids)
	assert.Equal(t, pairingB, c.pairingFor(sid(t, sidB1)))

	// A newer grouped set replaces the mapping.
	c.offerPairingShortIDs([]string{sidB1}, []PairingShortIDSet{{PairingID: pairingB, ShortIDs: []string{sidB1}}}, true, 500)
	assert.Equal(t, "", c.pairingFor(sid(t, sidA1)))
	assert.Equal(t, pairingB, c.pairingFor(sid(t, sidB1)))

	// An unknown response changes nothing.
	c.offerPairingShortIDs(nil, groupsAB(), false, 0)
	assert.Equal(t, "", c.pairingFor(sid(t, sidA1)))
}

func TestClient_HeartbeatGroupsAtTheSameRevision(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{sidA1, sidA2, sidB1}
	srv.registerResp.PairingShortIDsKnown = true
	srv.registerResp.PairingShortIDsRevision = 100
	srv.heartbeatRevision.Store(100)
	srv.heartbeatShortIDs.Store(&[]string{sidA1, sidA2, sidB1})
	sets := groupsAB()
	srv.heartbeatShortIDSets.Store(&sets)
	c := newTestClient(t, &fakeForwarder{}, &fakeBoxService{}, srv, func(cfg *Config) { cfg.HeartbeatInterval = 20 * time.Millisecond })
	require.NoError(t, c.Start(context.Background()))
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	require.Eventually(t, func() bool { return c.pairingFor(sid(t, sidB1)) == pairingB }, 2*time.Second, 10*time.Millisecond)
}

func TestClient_StartTakesRegisterGroups(t *testing.T) {
	srv := newStubServer(t)
	srv.registerResp.ServerConfig = launchCfgWithShortID()
	srv.registerResp.PairingShortIDs = []string{sidA1, sidA2, sidB1}
	srv.registerResp.PairingShortIDsKnown = true
	srv.registerResp.PairingShortIDSets = groupsAB()
	c := newTestClient(t, &fakeForwarder{}, &fakeBoxService{}, srv, func(cfg *Config) { cfg.HeartbeatInterval = time.Hour })
	require.NoError(t, c.Start(context.Background()))
	assert.Equal(t, pairingA, c.pairingFor(sid(t, sidA1)))
	require.NoError(t, c.Stop(context.Background()))
	assert.Equal(t, "", c.pairingFor(sid(t, sidA1)), "stop forgets the mapping")
}

func TestClient_ConnectionEventsCarryThePairing(t *testing.T) {
	live := helper.NewLiveTracker(nil)
	c := pairingClient(t, func(cfg *Config) { cfg.Live = live })
	c.offerPairingShortIDs([]string{sidA1, sidA2, sidB1}, groupsAB(), true, 100)

	got := make(chan ConnectionEvent, 16)
	sub := events.Subscribe(func(evt ConnectionEvent) { got <- evt })
	defer sub.Unsubscribe()
	next := func() ConnectionEvent {
		t.Helper()
		select {
		case evt := <-got:
			return evt
		case <-time.After(2 * time.Second):
			t.Fatal("no connection event")
			return ConnectionEvent{}
		}
	}

	friend := counter(0, 0)
	peerconn.Notify(peerconn.Event{State: +1, Source: "198.51.100.1:1000", Destination: "example.com:443", Bytes: friend, ShortID: sid(t, sidA2)})
	assert.Equal(t, pairingA, next().Pairing, "a pairing's short ID")
	peerconn.Notify(peerconn.Event{State: +1, Source: "198.51.100.2:1000", Destination: "example.com:443", Bytes: counter(0, 0), ShortID: sid(t, testBaseShortID)})
	assert.Equal(t, "", next().Pairing, "the launch config's own short ID is the pool")
	peerconn.Notify(peerconn.Event{State: +1, Source: "198.51.100.3:1000", Destination: "example.com:443", Bytes: counter(0, 0), ShortID: sid(t, "3333333333333333")})
	assert.Equal(t, "", next().Pairing, "an unknown short ID")
	peerconn.Notify(peerconn.Event{State: +1, Source: "198.51.100.4:1000", Destination: "example.com:443", Bytes: counter(0, 0)})
	assert.Equal(t, "", next().Pairing, "no short ID")

	// The snapshot labels the friend's IP for the globe.
	for _, pb := range c.RelaySnapshot().PeerBytes {
		if pb.IP == "198.51.100.1" {
			assert.Equal(t, pairingA, pb.Pairing)
		} else {
			assert.Equal(t, "", pb.Pairing, pb.IP)
		}
	}

	// And the LiveTracker sees the friend once real traffic moves.
	friend.AddDown(helper.LiveThreshold)
	s := live.Snapshot()
	require.Len(t, s.Pairings, 1)
	assert.Equal(t, pairingA, s.Pairings[0].PairingID)
	assert.True(t, s.Pairings[0].Connected)

	peerconn.Notify(peerconn.Event{State: -1, Source: "198.51.100.1:1000", Bytes: friend, ShortID: sid(t, sidA2)})
	evt := next()
	assert.Equal(t, -1, evt.State)
	assert.Equal(t, pairingA, evt.Pairing)

	data, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"pairing":"`+pairingA+`"`)
}

func TestClient_StopClosesLiveConnections(t *testing.T) {
	live := helper.NewLiveTracker(nil)
	c := pairingClient(t, func(cfg *Config) { cfg.Live = live })
	c.offerPairingShortIDs([]string{sidA1, sidA2, sidB1}, groupsAB(), true, 100)
	peerconn.Notify(peerconn.Event{State: +1, Source: "198.51.100.1:1000", Destination: "example.com:443", Bytes: counter(0, 0), ShortID: sid(t, sidB1)})
	require.Len(t, live.Snapshot().Pairings, 1)
	require.NoError(t, c.Stop(context.Background()))
	assert.Empty(t, live.Snapshot().Pairings, "an idle pairing whose connection Stop dropped is gone")
}

// Pairing IDs and short IDs never reach the logs: no slog call in the package
// is passed a pairing, a short ID, or a whole event that carries one.
func TestPairingIsNeverLogged(t *testing.T) {
	testutil.LoggedIdentifiers(t, ".", func(name string) bool {
		l := strings.ToLower(name)
		return strings.Contains(l, "pairing") || strings.Contains(l, "shortid") || name == "evt"
	})
}

// An accept callback that pauses after resolving its pairing, while Stop
// ends the session, must not add a connection once it resumes: Stop would
// have nothing left to close it, and its close is dropped while draining.
func TestClient_AcceptRacingStopLeavesNoLiveConnection(t *testing.T) {
	live := helper.NewLiveTracker(nil)
	c := pairingClient(t, func(cfg *Config) { cfg.Live = live })
	c.offerPairingShortIDs([]string{sidA1, sidA2, sidB1}, groupsAB(), true, 100)

	paused, resume := make(chan struct{}), make(chan struct{})
	c.beforeTrackLive = func() {
		close(paused)
		<-resume
	}
	notified := make(chan struct{})
	go func() {
		defer close(notified)
		peerconn.Notify(peerconn.Event{State: +1, Source: "198.51.100.1:1000", Destination: "example.com:443", Bytes: counter(0, 0), ShortID: sid(t, sidA1)})
	}()
	<-paused
	require.NoError(t, c.Stop(context.Background()))
	close(resume)
	<-notified

	assert.Empty(t, live.Snapshot().Pairings, "the stopped session's late accept was not tracked")
}
