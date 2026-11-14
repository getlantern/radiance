//go:build !novpn

package vpn

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	"github.com/miekg/dns"
	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badjson"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"

	rlog "github.com/getlantern/radiance/log"
)

func TestBaseOpts_TunIPv6Address(t *testing.T) {
	opts := baseOpts(t.TempDir())
	require.NotEmpty(t, opts.Inbounds, "expected inbounds in baseOpts output")

	var tunOpts *O.TunInboundOptions
	for _, in := range opts.Inbounds {
		if in.Type == "tun" {
			var ok bool
			tunOpts, ok = in.Options.(*O.TunInboundOptions)
			require.True(t, ok, "expected *TunInboundOptions for tun inbound")
			break
		}
	}
	require.NotNil(t, tunOpts, "expected a tun inbound")
	require.NotEmpty(t, tunOpts.Address, "expected at least the v4 TUN address")
	assert.Equal(t, "10.10.1.1/30", tunOpts.Address[0].String(), "first TUN address should be the v4 prefix")

	if hasGlobalIPv6() {
		require.Len(t, tunOpts.Address, 2, "expected v4 + v6 ULA on TUN when system has global v6")
		assert.Equal(t, "fdfe:dcba:9876::1/126", tunOpts.Address[1].String(),
			"v6 ULA should be appended after the v4 address")
	} else {
		require.Len(t, tunOpts.Address, 1, "expected v4-only TUN when system has no global v6")
	}
}

func TestBaseRoutingRules_RejectsTunSelfDial(t *testing.T) {
	rules := baseRoutingRules()
	selfIdx, privateIdx := -1, -1
	for i, r := range rules {
		raw := r.DefaultOptions.RawDefaultRule
		if r.DefaultOptions.RuleAction.Action == C.RuleActionTypeReject &&
			slices.Contains(raw.IPCIDR, "10.10.1.0/30") {
			selfIdx = i
			assert.Equal(t, []string{inboundTag}, []string(raw.Inbound))
			assert.Contains(t, []string(raw.IPCIDR), "fdfe:dcba:9876::/126")
		}
		if raw.IPIsPrivate && privateIdx == -1 {
			privateIdx = i
		}
	}
	require.NotEqual(t, -1, selfIdx, "expected a reject rule for the TUN subnet")
	require.NotEqual(t, -1, privateIdx, "expected the private-IP direct rule")
	assert.Less(t, selfIdx, privateIdx, "TUN self-dial reject must precede the private-IP direct rule")
}

func TestKernelBelow(t *testing.T) {
	tests := []struct {
		name string
		v    string
		min  string
		want bool
	}{
		{"below major", "4.19.0", "5.10", true},
		{"below minor", "5.4.0", "5.10", true},
		{"equal", "5.10.0", "5.10", false},
		{"above minor", "5.15.0", "5.10", false},
		{"above major", "6.1.0", "5.10", false},
		{"android suffix", "4.19.0-android13", "5.10", true},
		{"android suffix above", "5.15.0-android13", "5.10", false},
		{"empty version", "", "5.10", false},
		{"empty min", "5.10.0", "", false},
		{"both empty", "", "", false},
		{"invalid version", "not-a-version", "5.10", false},
		{"only major", "5", "5.10", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, kernelBelow(tt.v, tt.min))
		})
	}
}

func dnsServer(opts O.Options, tag string) (O.DNSServerOptions, int) {
	var found O.DNSServerOptions
	count := 0
	for _, s := range opts.DNS.Servers {
		if s.Tag == tag {
			found = s
			count++
		}
	}
	return found, count
}

func TestBuildOptions_SplitTunnelResolvesWithSystemDNS(t *testing.T) {
	// Spare capacity lets an in-place append show up in the config's backing array.
	serverDNS := func(servers ...O.DNSServerOptions) *O.DNSOptions {
		return &O.DNSOptions{RawDNSOptions: O.RawDNSOptions{Servers: append(make([]O.DNSServerOptions, 0, 8), servers...)}}
	}
	remote := O.DNSServerOptions{Type: C.DNSTypeLocal, Tag: "dns_remote", Options: &O.LocalDNSServerOptions{}}
	tests := []struct {
		name string
		dns  *O.DNSOptions
	}{
		{name: "base DNS"},
		{name: "config DNS", dns: serverDNS(remote)},
		{name: "config DNS with system server", dns: serverDNS(remote, splitTunnelDNSServers()[0])},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options, _ := testBoxOptions(t)
			options.DNS = tt.dns
			var configServers int
			if tt.dns != nil {
				configServers = len(tt.dns.Servers)
			}

			opts, _, err := buildOptions(BoxOptions{BasePath: t.TempDir(), Options: options})
			require.NoError(t, err)

			server, count := dnsServer(opts, systemDNSTag)
			assert.Equal(t, 1, count, "exactly one system DNS server")
			assert.Equal(t, C.DNSTypeLocal, server.Type)
			if tt.dns != nil {
				spare := tt.dns.Servers[configServers:cap(tt.dns.Servers)]
				assert.Equal(t, make([]O.DNSServerOptions, len(spare)), spare, "config DNS servers must not be modified")
			}

			require.GreaterOrEqual(t, len(opts.DNS.Rules), 3)
			evaluate := opts.DNS.Rules[0].DefaultOptions.DNSRuleAction
			assert.Equal(t, C.RuleActionTypeEvaluate, evaluate.Action, "split-tunnel evaluate must come first")
			assert.Equal(t, systemDNSTag, evaluate.EvaluateOptions.Server)
			assert.Equal(t, badoption.Duration(systemDNSEvaluateTimeout), evaluate.EvaluateOptions.Timeout)
			assert.Equal(t, C.RuleActionTypeRespond, opts.DNS.Rules[1].DefaultOptions.DNSRuleAction.Action)
			assert.Equal(t, C.RuleActionTypePredefined, opts.DNS.Rules[2].DefaultOptions.DNSRuleAction.Action,
				"AAAA suppression must follow the split-tunnel DNS rules")

			ruleIdx := slices.IndexFunc(opts.Route.Rules, func(r O.Rule) bool {
				return slices.Contains(r.DefaultOptions.RuleSet, splitTunnelTag)
			})
			require.NotEqual(t, -1, ruleIdx, "split-tunnel rules missing")
			require.Less(t, ruleIdx+1, len(opts.Route.Rules))
			resolve := opts.Route.Rules[ruleIdx].DefaultOptions.RuleAction
			assert.Equal(t, C.RuleActionTypeResolve, resolve.Action)
			assert.Equal(t, systemDNSTag, resolve.ResolveOptions.Server)
			assert.Equal(t, O.DomainStrategy(C.DomainStrategyPreferIPv4), resolve.ResolveOptions.Strategy)

			route := opts.Route.Rules[ruleIdx+1].DefaultOptions
			assert.Equal(t, []string{splitTunnelTag}, []string(route.RuleSet))
			assert.Equal(t, C.RuleActionTypeRoute, route.RuleAction.Action)
			assert.Equal(t, "direct", route.RuleAction.RouteOptions.Outbound)
		})
	}
}

// TestSplitTunnelDNSEndToEnd runs queries through a started box's DNS router,
// with the system resolver replaced by fixed answers. A split-tunnel domain
// gets its real address only when it is private; otherwise it gets a fake IP
// that maps back to the domain, so routing doesn't depend on sniffing.
func TestSplitTunnelDNSEndToEnd(t *testing.T) {
	dir := t.TempDir()
	st := newSplitTunnel(dir, rlog.NoOpLogger())
	require.NoError(t, st.AddItem(TypeDomainSuffix, "example.com"))
	require.NoError(t, st.SetEnabled(true))

	options, _ := testBoxOptions(t)
	opts, _, err := buildOptions(BoxOptions{BasePath: dir, Options: options})
	require.NoError(t, err)

	answers := map[string]string{
		"intranet.example.com": "192.168.9.138",
		"public.example.com":   "203.0.113.10",
		"other.org":            "198.51.100.1",
	}
	predefined := new(badjson.TypedMap[string, badoption.Listable[netip.Addr]])
	for domain, addr := range answers {
		predefined.Put(domain, badoption.Listable[netip.Addr]{netip.MustParseAddr(addr)})
	}
	i := slices.IndexFunc(opts.DNS.Servers, func(s O.DNSServerOptions) bool { return s.Tag == systemDNSTag })
	require.NotEqual(t, -1, i)
	opts.DNS.Servers[i] = O.DNSServerOptions{
		Type:    C.DNSTypeHosts,
		Tag:     systemDNSTag,
		Options: &O.HostsDNSServerOptions{Predefined: predefined},
	}
	opts.Inbounds = nil
	opts.Log = &O.LogOptions{Disabled: true}
	opts.Experimental = nil

	ctx := box.Context(context.Background())
	instance, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	require.NoError(t, instance.Start())
	t.Cleanup(func() { instance.Close() })

	router := service.FromContext[adapter.DNSRouter](ctx)
	fakeIP := service.FromContext[adapter.DNSTransportManager](ctx).FakeIP().Store()
	resolve := func(domain string) netip.Addr {
		t.Helper()
		query := new(dns.Msg)
		query.SetQuestion(dns.Fqdn(domain), dns.TypeA)
		response, err := router.Exchange(adapter.WithContext(ctx, &adapter.InboundContext{}), query, adapter.DNSQueryOptions{})
		require.NoError(t, err)
		require.Len(t, response.Answer, 1)
		a, ok := response.Answer[0].(*dns.A)
		require.True(t, ok)
		addr, _ := netip.AddrFromSlice(a.A)
		return addr.Unmap()
	}

	assert.Equal(t, netip.MustParseAddr("192.168.9.138"), resolve("intranet.example.com"),
		"private split-tunnel answer is returned as-is")
	for _, domain := range []string{"public.example.com", "other.org"} {
		addr := resolve(domain)
		require.True(t, fakeIP.Contains(addr), "%s should get a fake IP, got %s", domain, addr)
		mapped, ok := fakeIP.Lookup(addr)
		require.True(t, ok)
		assert.Equal(t, domain, mapped, "fake IP must map back to the queried domain")
	}
}
