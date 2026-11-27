package vpn

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	singjson "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal/hostaccess"
)

func TestNormalizeLocale(t *testing.T) {
	tests := []struct {
		name     string
		locale   string
		expected string
	}{
		{
			name:     "lowercase with hyphen",
			locale:   "zh-cn",
			expected: "ZHCN",
		},
		{
			name:     "lowercase with underscore",
			locale:   "ru_ru",
			expected: "RURU",
		},
		{
			name:     "mixed case with hyphen",
			locale:   "en-US",
			expected: "ENUS",
		},
		{
			name:     "all uppercase",
			locale:   "FAIR",
			expected: "FAIR",
		},
		{
			name:     "all lowercase",
			locale:   "fair",
			expected: "FAIR",
		},
		{
			name:     "multiple hyphens and underscores",
			locale:   "en-US_test",
			expected: "ENUSTEST",
		},
		{
			name:     "empty string",
			locale:   "",
			expected: "",
		},
		{
			name:     "only hyphens and underscores",
			locale:   "-_-_",
			expected: "",
		},
		{
			name:     "numbers and letters",
			locale:   "abc123",
			expected: "ABC123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeLocale(tt.locale)
			assert.Equalf(t, tt.expected, result, "normalizeLocale(%q) should return %q", tt.locale, tt.expected)
		})
	}
}
func TestLocalResolvers(t *testing.T) {
	names := func(resolvers []dohResolver) []string {
		var out []string
		for _, r := range resolvers {
			out = append(out, r.name)
		}
		return out
	}
	edges := names(cloudflareEdgeDoH)
	tests := []struct {
		locale string
		want   []string
	}{
		{"CN", slices.Concat([]string{"alidns"}, edges)},
		{"IR", slices.Concat([]string{"cloudflare", "alidns"}, edges)},
		{"FAIR", slices.Concat([]string{"cloudflare", "alidns"}, edges)},
		{"fair", slices.Concat([]string{"cloudflare", "alidns"}, edges)},
		{"ZHCN", slices.Concat([]string{"alidns"}, edges)},
		{"zh-cn", slices.Concat([]string{"alidns"}, edges)},
		{"zh_cn", slices.Concat([]string{"alidns"}, edges)},
		{"RURU", slices.Concat([]string{"yandex"}, edges)},
		{"ru-ru", slices.Concat([]string{"yandex"}, edges)},
		{"RU", slices.Concat([]string{"yandex"}, edges)},
		{"enus", slices.Concat([]string{"cloudflare", "google"}, edges)},
		{"en-US", slices.Concat([]string{"cloudflare", "google"}, edges)},
		{"fr-FR", slices.Concat([]string{"cloudflare", "google"}, edges)},
		{"", slices.Concat([]string{"cloudflare", "google"}, edges)},
	}
	for _, tt := range tests {
		t.Run(tt.locale, func(t *testing.T) {
			t.Cleanup(settings.Reset)
			settings.Set(settings.LocaleKey, tt.locale)
			got, stagger := localResolvers()
			assert.Equal(t, tt.want, names(got))
			if _, censored := map[string]bool{"IR": true, "FAIR": true, "ZHCN": true, "CN": true, "RU": true, "RURU": true}[normalizeLocale(tt.locale)]; censored {
				assert.Equal(t, time.Second, stagger, "a censored locale's first resolver gets a full second")
			} else {
				assert.Zero(t, stagger, "the pool's default stagger")
			}
			for _, r := range got {
				assert.NotEqual(t, "9.9.9.9", r.ip, "Quad9 is not a local resolver: some networks block its DoH")
			}
		})
	}
}

func TestLocalDNSServersArePooled(t *testing.T) {
	servers := localDNSServers(slices.Concat([]dohResolver{cloudflareDoH}, cloudflareEdgeDoH), time.Second)
	require.NotEmpty(t, servers)
	assert.Equal(t, "dns_local", servers[0].Tag)
	assert.Equal(t, lbC.TypeDNSPool, servers[0].Type)
	pool := servers[0].Options.(*lbO.DNSPoolServerOptions)
	assert.Equal(t, badoption.Duration(time.Second), pool.Stagger)
	require.Len(t, pool.Servers, len(servers)-1)
	for i, tag := range pool.Servers {
		server := servers[i+1]
		assert.Equal(t, tag, server.Tag)
		assert.Equal(t, constant.DNSTypeHTTPS, server.Type)
		options := server.Options.(*option.RemoteHTTPSDNSServerOptions)
		assert.Empty(t, options.Detour, "dns_local's servers dial directly")
	}
	edge := servers[2].Options.(*option.RemoteHTTPSDNSServerOptions)
	assert.Equal(t, "104.16.249.249", edge.Server)
	require.NotNil(t, edge.TLS)
	assert.Equal(t, "cloudflare-dns.com", edge.TLS.ServerName, "an edge IP is reached with cloudflare-dns.com's name")
	plain := servers[1].Options.(*option.RemoteHTTPSDNSServerOptions)
	assert.Nil(t, plain.TLS, "1.1.1.1's certificate covers its IP")
}

// TestDNSServersBuildABox checks the DNS servers survive a JSON round trip
// and start in a box built the way radiance builds one.
func TestDNSServersBuildABox(t *testing.T) {
	for _, locale := range []string{"en-US", "zh-cn", "ru-ru"} {
		t.Run(locale, func(t *testing.T) {
			t.Cleanup(settings.Reset)
			settings.Set(settings.LocaleKey, locale)
			options := option.Options{
				Log: &option.LogOptions{Disabled: true},
				DNS: &option.DNSOptions{RawDNSOptions: option.RawDNSOptions{
					Servers: buildDNSServers(),
					Rules:   buildDNSRules(),
					Final:   "dns_local",
				}},
				// As radiance resolves for the direct and bypass paths.
				Route: &option.RouteOptions{DefaultDomainResolver: &option.DomainResolveOptions{Server: "dns_local"}},
				// dns_remote dials through "auto"; any outbound will do here.
				Outbounds: []option.Outbound{{Type: constant.TypeBlock, Tag: "auto", Options: &option.StubOptions{}}},
			}
			var local *option.DNSServerOptions
			for i, s := range options.DNS.Servers {
				if s.Tag == "dns_local" {
					local = &options.DNS.Servers[i]
				}
			}
			require.NotNil(t, local)
			assert.Equal(t, lbC.TypeDNSPool, local.Type, "dns_local must be a pool, so one blocked resolver can't take it down")
			require.NoError(t, hostaccess.CheckOptions(options))
			ctx := hostaccess.Context(context.Background())
			raw, err := singjson.Marshal(options)
			require.NoError(t, err)
			parsed, err := singjson.UnmarshalExtendedContext[option.Options](ctx, raw)
			require.NoError(t, err)
			instance, err := sbox.New(sbox.Options{Context: ctx, Options: parsed})
			require.NoError(t, err)
			require.NoError(t, instance.Start())
			require.NoError(t, instance.Close())
		})
	}
}

func TestBuildDNSRules_SuppressesAAAA(t *testing.T) {
	rules := buildDNSRules()

	var fakeIP, aaaa *option.DefaultDNSRule
	for i := range rules {
		d := &rules[i].DefaultOptions
		switch {
		case d.Action == constant.RuleActionTypeRoute && d.RouteOptions.Server == "dns_fakeip":
			fakeIP = d
		case slices.Contains(d.QueryType, option.DNSQueryType(dns.TypeAAAA)):
			aaaa = d
		}
	}

	require.NotNil(t, fakeIP, "expected a fake-IP route rule")
	assert.Contains(t, fakeIP.QueryType, option.DNSQueryType(dns.TypeA), "fake-IP rule should handle A queries")
	assert.NotContains(t, fakeIP.QueryType, option.DNSQueryType(dns.TypeAAAA), "fake-IP rule must not handle AAAA — AAAA is suppressed")

	require.NotNil(t, aaaa, "expected an AAAA suppression rule")
	assert.Equal(t, constant.RuleActionTypePredefined, aaaa.Action, "AAAA rule should use the predefined action")
	require.NotNil(t, aaaa.PredefinedOptions.Rcode, "AAAA predefined rule must set an rcode")
	assert.Equal(t, dns.RcodeSuccess, int(*aaaa.PredefinedOptions.Rcode), "AAAA suppression must return NODATA (NOERROR), not NXDOMAIN")
	assert.Empty(t, aaaa.PredefinedOptions.Answer, "AAAA suppression must return no answer records (NODATA)")
}
