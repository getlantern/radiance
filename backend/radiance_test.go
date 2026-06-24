package backend

import (
	"testing"
	"time"

	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/servers"
)

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
