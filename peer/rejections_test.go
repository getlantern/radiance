package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/getlantern/lantern-box/tracker/peerconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRejectionTally_GroupsByRegistrableDomain(t *testing.T) {
	tally := newRejectionTally("")
	tally.record("rr1---sn-abc.googlevideo.com:443")
	tally.record("rr2---sn-def.googlevideo.com:443")
	tally.record("RR2---SN-DEF.googlevideo.com.:443")
	tally.record("scontent-iad3-1.cdninstagram.com:443")
	tally.record("149.154.167.51:443")
	tally.record("[2001:b28:f23d:f001::a]:5222")
	tally.record("bare-host.example")
	tally.record(":443")

	domains, total := tally.summary()
	assert.Equal(t, int64(7), total)
	assert.Equal(t, rejectedDomain{Domain: "googlevideo.com", Count: 3, Hosts: 2}, domains[0])
	assert.Contains(t, domains, rejectedDomain{Domain: "cdninstagram.com", Count: 1, Hosts: 1})
	assert.Contains(t, domains, rejectedDomain{Domain: "149.154.167.51", Count: 1, Hosts: 1})
	assert.Contains(t, domains, rejectedDomain{Domain: "2001:b28:f23d:f001::a", Count: 1, Hosts: 1})
	assert.Contains(t, domains, rejectedDomain{Domain: "bare-host.example", Count: 1, Hosts: 1},
		"a destination without a port is still counted")

	h := tally.state.Hosts["rr2---sn-def.googlevideo.com"]
	require.NotNil(t, h, "host case and trailing dot are normalized")
	assert.Equal(t, map[string]int64{"443": 2}, h.Ports)
	assert.Equal(t, map[string]int64{"5222": 1}, tally.state.Hosts["2001:b28:f23d:f001::a"].Ports)
}

func TestRejectionTally_CapsHostsAndCountsOverflow(t *testing.T) {
	tally := newRejectionTally("")
	for i := range maxRejectedHosts + 5 {
		tally.record(fmt.Sprintf("h%d.example.com:443", i))
	}
	tally.record("h0.example.com:443")
	assert.Len(t, tally.state.Hosts, maxRejectedHosts)
	assert.Equal(t, int64(5), tally.state.Overflow)
	assert.Equal(t, int64(2), tally.state.Hosts["h0.example.com"].Count, "known hosts still count past the cap")
	_, total := tally.summary()
	assert.Equal(t, int64(maxRejectedHosts+6), total)
}

func TestRejectionTally_PersistsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer-rejections.json")
	tally := newRejectionTally(path)
	tally.flush()
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "an unchanged tally is not written")

	tally.record("pbs.twimg.com:443")
	tally.record("pbs.twimg.com:443")
	tally.flush()

	reloaded := newRejectionTally(path)
	domains, total := reloaded.summary()
	assert.Equal(t, int64(2), total)
	assert.Equal(t, []rejectedDomain{{Domain: "twimg.com", Count: 2, Hosts: 1}}, domains)

	reloaded.record("pbs.twimg.com:443")
	reloaded.flush()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var onDisk rejectionState
	require.NoError(t, json.Unmarshal(raw, &onDisk))
	assert.Equal(t, int64(3), onDisk.Hosts["pbs.twimg.com"].Count)

	leftovers, err := filepath.Glob(path + ".tmp*")
	require.NoError(t, err)
	assert.Empty(t, leftovers)
}

func TestRejectionTally_CorruptFileStartsFresh(t *testing.T) {
	tooMany := `{"hosts":{`
	for i := range maxRejectedHosts + 1 {
		if i > 0 {
			tooMany += ","
		}
		tooMany += fmt.Sprintf(`"h%d.example.com":{"count":1,"ports":{}}`, i)
	}
	tooMany += `}}`
	for name, saved := range map[string]string{
		"not json":       `{not json`,
		"null host":      `{"hosts":{"x.example.com":null}}`,
		"null ports":     `{"hosts":{"x.example.com":{"count":3,"ports":null}}}`,
		"negative count": `{"hosts":{"x.example.com":{"count":-1,"ports":{}}}}`,
		"too many hosts": tooMany,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "peer-rejections.json")
			require.NoError(t, os.WriteFile(path, []byte(saved), 0o600))
			tally := newRejectionTally(path)
			tally.record("x.example.com:443")
			domains, total := tally.summary()
			assert.Equal(t, int64(1), total)
			assert.Equal(t, []rejectedDomain{{Domain: "example.com", Count: 1, Hosts: 1}}, domains)
		})
	}
}

// A port scan against one host must not grow its breakdown without bound.
func TestRejectionTally_CapsPortsPerHost(t *testing.T) {
	tally := newRejectionTally("")
	for port := range maxPortsPerHost + 10 {
		tally.record(fmt.Sprintf("scanned.example.com:%d", 1000+port))
	}
	tally.record("scanned.example.com:1000")
	h := tally.state.Hosts["scanned.example.com"]
	assert.Len(t, h.Ports, maxPortsPerHost+1)
	assert.Equal(t, int64(10), h.Ports[otherPorts])
	assert.Equal(t, int64(2), h.Ports["1000"], "known ports still count past the cap")
	assert.Equal(t, int64(maxPortsPerHost+11), h.Count)
}

func TestRejectionTally_FailedWriteRetriesOnNextFlush(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	tally := newRejectionTally(filepath.Join(dir, "peer-rejections.json"))
	tally.record("example.com:443")
	tally.flush()
	assert.True(t, tally.dirty, "a failed write keeps the tally dirty")

	require.NoError(t, os.Mkdir(dir, 0o700))
	tally.flush()
	assert.False(t, tally.dirty)
	_, err := os.Stat(filepath.Join(dir, "peer-rejections.json"))
	assert.NoError(t, err)
}

// Only close events the inbound marked Rejected reach the tally, and Stop
// flushes it so a session's refusals are not lost.
func TestClient_TalliesRejectedCloseEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer-rejections.json")
	fwd := &fakeForwarder{externalIP: "203.0.113.42"}
	c := newTestClient(t, fwd, &fakeBoxService{}, newStubServer(t), func(cfg *Config) {
		cfg.RejectionsPath = path
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx))

	peerconn.Notify(peerconn.Event{State: +1, Source: "198.51.100.9:5000", Destination: "allowed.example:443"})
	peerconn.Notify(peerconn.Event{State: -1, Source: "198.51.100.9:5000"})
	peerconn.Notify(peerconn.Event{State: +1, Source: "198.51.100.9:5001", Destination: "rr1---sn-abc.googlevideo.com:443"})
	peerconn.Notify(peerconn.Event{State: -1, Source: "198.51.100.9:5001", Destination: "rr1---sn-abc.googlevideo.com:443", Rejected: true})
	require.NoError(t, c.Stop(ctx))

	domains, total := newRejectionTally(path).summary()
	assert.Equal(t, int64(1), total)
	assert.Equal(t, []rejectedDomain{{Domain: "googlevideo.com", Count: 1, Hosts: 1}}, domains)
}
