//go:build !novpn

package vpn

import (
	"context"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	box "github.com/getlantern/lantern-box"
	"github.com/miekg/dns"
	sbox "github.com/sagernet/sing-box"
	A "github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	R "github.com/sagernet/sing-box/route/rule"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitPolicyPreservesCloudDNS(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, policy := range []SplitTunnelPolicy{SplitTunnelPolicyExclude, SplitTunnelPolicyInclude} {
			state := SplitTunnelState{Enabled: enabled, Policy: policy}
			opts := splitPolicyTestOptions(t, state)
			ctx := splitPolicyTestContext(t, opts, AutoSelectTag)
			assert.Equal(t, "dns_remote", opts.DNS.Final)
			assert.Equal(t, "dns_remote", opts.Route.DefaultDomainResolver.Server)
			direct := findOutbound(opts.Outbounds, "direct").Options.(*O.DirectOutboundOptions)
			if enabled {
				require.NotNil(t, direct.DomainResolver)
				assert.Equal(t, "dns_local", direct.DomainResolver.Server)
				assert.True(t, opts.DNS.IndependentCache)
			} else {
				assert.Nil(t, direct.DomainResolver)
			}
			for _, server := range opts.DNS.Servers {
				if server.Tag == "dns_remote" {
					assert.Equal(t, "auto", server.Options.(*O.RemoteHTTPSDNSServerOptions).Detour)
				}
			}
			for _, domain := range []string{"selected.test", "selected.cn"} {
				for _, queryType := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeHTTPS, dns.TypeSVCB, dns.TypeTXT, dns.TypeMX} {
					metadata := A.InboundContext{Domain: domain, QueryType: queryType}
					var matched A.RuleAction
					for _, ruleOptions := range opts.DNS.Rules {
						rule, err := R.NewDNSRule(ctx, log.NewNOPFactory().Logger(), ruleOptions, false)
						require.NoError(t, err)
						require.NoError(t, rule.Start())
						if rule.Match(&metadata) {
							matched = rule.Action()
						}
						require.NoError(t, rule.Close())
						if matched != nil {
							break
						}
					}
					switch queryType {
					case dns.TypeA:
						require.IsType(t, &R.RuleActionDNSRoute{}, matched)
						assert.Equal(t, "dns_fakeip", matched.(*R.RuleActionDNSRoute).Server)
					case dns.TypeAAAA, dns.TypeHTTPS, dns.TypeSVCB:
						require.IsType(t, &R.RuleActionPredefined{}, matched)
					default:
						if domain == "selected.cn" {
							require.IsType(t, &R.RuleActionDNSRoute{}, matched)
							assert.Equal(t, "dns_local", matched.(*R.RuleActionDNSRoute).Server)
						} else {
							assert.Nil(t, matched, "queries without app ownership retain the cloud fallback")
						}
					}
				}
			}
		}
	}
}

func splitPolicyCloudDNSRules() []O.DNSRule {
	fakeIP := fakeipRule()
	fakeIP.DefaultOptions.QueryType = append(fakeIP.DefaultOptions.QueryType, O.DNSQueryType(dns.TypeAAAA))
	noError := O.DNSRCode(dns.RcodeSuccess)
	return []O.DNSRule{fakeIP, {Type: C.RuleTypeDefault, DefaultOptions: O.DefaultDNSRule{
		RawDefaultDNSRule: O.RawDefaultDNSRule{QueryType: []O.DNSQueryType{O.DNSQueryType(dns.TypeHTTPS), O.DNSQueryType(dns.TypeSVCB)}},
		DNSRuleAction: O.DNSRuleAction{Action: C.RuleActionTypePredefined,
			PredefinedOptions: O.DNSRouteActionPredefined{Rcode: &noError}},
	}}, {Type: C.RuleTypeDefault, DefaultOptions: O.DefaultDNSRule{
		RawDefaultDNSRule: O.RawDefaultDNSRule{RuleSet: []string{"geosite-cn"}},
		DNSRuleAction: O.DNSRuleAction{Action: C.RuleActionTypeRoute,
			RouteOptions: O.DNSRouteActionOptions{Server: "dns_local"}},
	}}}
}

func TestSplitPolicyAddsMissingDirectResolver(t *testing.T) {
	cfg := testConfig(t)
	cfg.Options.DNS = &O.DNSOptions{RawDNSOptions: O.RawDNSOptions{
		Servers: []O.DNSServerOptions{{Type: C.DNSTypeLocal, Tag: "custom", Options: &O.LocalDNSServerOptions{}}},
		Final:   "custom",
	}}
	opts, err := buildOptions(BoxOptions{
		BasePath: t.TempDir(), Options: cfg.Options,
		SplitTunnel: SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude},
	})
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(opts.DNS.Servers, func(server O.DNSServerOptions) bool { return server.Tag == "dns_local" }))
	assert.Len(t, cfg.Options.DNS.Servers, 1)
	assert.Equal(t, "custom", opts.DNS.Final)
}

func TestSplitDirectResolvesLocallyAfterFakeIP(t *testing.T) {
	local, localQueries := splitPolicyDNSServer(t, "dns_local")
	remote, remoteQueries := splitPolicyDNSServer(t, "dns_remote")
	opts := splitPolicyTestOptions(t, SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude})
	opts.Inbounds = nil
	opts.Experimental = nil
	opts.Log = &O.LogOptions{Disabled: true}
	opts.Outbounds = []O.Outbound{*findOutbound(opts.Outbounds, "direct")}
	opts.Route = &O.RouteOptions{DefaultDomainResolver: &O.DomainResolveOptions{Server: "dns_remote"}, RuleSet: []O.RuleSet{splitPolicyCountryRuleSet()}}
	opts.DNS.Servers = []O.DNSServerOptions{local, remote, fakeipServer()}
	ctx := box.Context(t.Context())
	instance, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, instance.Close()) })
	require.NoError(t, instance.Start())

	router := service.FromContext[A.DNSRouter](ctx)
	require.NotNil(t, router)
	query := new(dns.Msg).SetQuestion("selected.test.", dns.TypeA)
	response, err := router.Exchange(t.Context(), query, A.DNSQueryOptions{})
	require.NoError(t, err)
	require.Len(t, response.Answer, 1)
	assert.True(t, net.ParseIP("198.18.0.0").Equal(response.Answer[0].(*dns.A).A.Mask(net.CIDRMask(15, 32))))
	remoteOptions, err := A.DNSQueryOptionsFrom(ctx, &O.DomainResolveOptions{Server: "dns_remote"})
	require.NoError(t, err)
	_, err = router.Exchange(t.Context(), query, *remoteOptions)
	require.NoError(t, err)
	require.Positive(t, remoteQueries.Load())
	remoteCount := remoteQueries.Load()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	outbound, ok := instance.Outbound().Outbound("direct")
	require.True(t, ok)
	conn, err := outbound.DialContext(ctx, "tcp", M.Socksaddr{Fqdn: "selected.test", Port: uint16(listener.Addr().(*net.TCPAddr).Port)})
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	assert.Positive(t, localQueries.Load())
	assert.Equal(t, remoteCount, remoteQueries.Load())
	localCount := localQueries.Load()
	conn, err = outbound.DialContext(ctx, "tcp", M.Socksaddr{Fqdn: "selected.test", Port: uint16(listener.Addr().(*net.TCPAddr).Port)})
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	assert.Equal(t, localCount, localQueries.Load())
	assert.Equal(t, remoteCount, remoteQueries.Load())
}

func splitPolicyDNSServer(t *testing.T, tag string) (O.DNSServerOptions, *atomic.Int32) {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	queries := new(atomic.Int32)
	started := make(chan struct{})
	server := &dns.Server{PacketConn: packet, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
		response := new(dns.Msg).SetReply(query)
		if query.Question[0].Qtype == dns.TypeA {
			queries.Add(1)
			response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("127.0.0.1")}}
		}
		_ = w.WriteMsg(response)
	})}
	done := make(chan error, 1)
	go func() { done <- server.ActivateAndServe() }()
	<-started
	t.Cleanup(func() {
		assert.NoError(t, server.Shutdown())
		assert.NoError(t, <-done)
	})
	return O.DNSServerOptions{Tag: tag, Type: C.DNSTypeUDP, Options: &O.RemoteDNSServerOptions{
		DNSServerAddressOptions: O.DNSServerAddressOptions{Server: "127.0.0.1", ServerPort: uint16(packet.LocalAddr().(*net.UDPAddr).Port)},
	}}, queries
}
