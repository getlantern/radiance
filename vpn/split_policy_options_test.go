//go:build !novpn

package vpn

import (
	"context"
	"net/netip"
	"testing"

	A "github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	R "github.com/sagernet/sing-box/route/rule"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/bypass"
	rlog "github.com/getlantern/radiance/log"
)

func TestSplitPolicyRouting(t *testing.T) {
	filters := SplitTunnelFilter{
		DomainSuffix: []string{"selected.ir", "selected.cn", "blocked.test", "ai.test"},
		ProcessPath:  []string{"/apps/browser"},
	}
	for _, tt := range []struct {
		name    string
		enabled bool
		policy  SplitTunnelPolicy
		filters SplitTunnelFilter
		domain  string
		process string
		address string
		inbound string
		mode    string
		want    string
	}{
		{name: "off keeps country bypass", policy: SplitTunnelPolicyInclude, filters: filters, domain: "selected.ir", want: "direct"},
		{name: "off keeps cloud country bypass", policy: SplitTunnelPolicyInclude, filters: filters, domain: "selected.cn", want: "direct"},
		{name: "bypass selected", enabled: true, policy: SplitTunnelPolicyExclude, filters: filters, domain: "selected.ir", want: "direct"},
		{name: "bypass other", enabled: true, policy: SplitTunnelPolicyExclude, filters: filters, domain: "other.test", want: "auto"},
		{name: "include overrides country bypass", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, domain: "selected.ir", want: "auto"},
		{name: "include overrides cloud country bypass", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, domain: "selected.cn", want: "auto"},
		{name: "include other", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, domain: "other.test", want: "direct"},
		{name: "include overrides cloud proxy exception", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, domain: "google.cn", want: "direct"},
		{name: "include retains selected cloud proxy exception", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, domain: "google.cn", process: "/apps/browser", want: "auto"},
		{name: "include app or website", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, domain: "other.test", process: "/apps/browser", mode: ManualSelectTag, want: "manual"},
		{name: "include app without domain", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, process: "/apps/browser", address: "8.8.8.8", want: "auto"},
		{name: "include missing metadata", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, address: "8.8.8.8", want: "direct"},
		{name: "include preserves proxy category", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, domain: "ai.test", want: "sr-openai"},
		{name: "include preserves ad block", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, domain: "blocked.test", want: C.RuleActionTypeReject},
		{name: "include preserves private networks", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, process: "/apps/browser", address: "192.168.1.1", want: "direct"},
		{name: "include preserves bootstrap inbound", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, domain: "selected.ir", inbound: bypass.BypassInboundTag, want: "direct"},
		{name: "include preserves DNS bootstrap", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, process: "/apps/browser", address: "223.5.5.5", want: "direct"},
		{name: "include preserves rejection", enabled: true, policy: SplitTunnelPolicyInclude, filters: filters, domain: "selected.ir", mode: rejectMode, want: C.RuleActionTypeReject},
		{name: "empty include", enabled: true, policy: SplitTunnelPolicyInclude, domain: "google.cn", want: "direct"},
		{name: "empty bypass", enabled: true, policy: SplitTunnelPolicyExclude, domain: "other.test", want: "auto"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := SplitTunnelState{Enabled: tt.enabled, Policy: tt.policy, Filters: tt.filters}
			opts := splitPolicyTestOptions(t, state)
			mode := tt.mode
			if mode == "" {
				mode = AutoSelectTag
			}
			rules := splitPolicyTestRules(t, opts, mode)
			for _, network := range []string{"tcp", "udp"} {
				metadata := A.InboundContext{Inbound: tt.inbound, Network: network, Domain: tt.domain, IPVersion: 4}
				if tt.process != "" {
					metadata.ProcessInfo = &A.ConnectionOwner{ProcessPath: tt.process}
				}
				if tt.address != "" {
					metadata.Destination = M.Socksaddr{Addr: netip.MustParseAddr(tt.address), Port: 443}
				}
				assert.Equal(t, tt.want, splitPolicyRoute(rules, metadata), network)
			}
		})
	}
}

func TestSplitPolicyRemovesDirectFromMixedSmartGroup(t *testing.T) {
	cfg := testConfig(t)
	cfg.SmartRouting[1].Outbounds = []string{" direct ", "http1-out"}
	options := BoxOptions{
		BasePath: t.TempDir(), Options: cfg.Options, SmartRouting: cfg.SmartRouting,
		SplitTunnel: SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude},
	}
	opts, err := buildOptions(options)
	require.NoError(t, err)
	outbound := findOutbound(opts.Outbounds, "sr-openai")
	require.NotNil(t, outbound)
	assert.Equal(t, []string{"http1-out"}, []string(outbound.Options.(*O.URLTestOutboundOptions).Outbounds))
	assert.Equal(t, []string{" direct ", "http1-out"}, cfg.SmartRouting[1].Outbounds)
	assert.Equal(t, []string{"direct"}, cfg.SmartRouting[0].Outbounds)
}

func TestSplitPolicyIPv6Fallback(t *testing.T) {
	opts := splitPolicyTestOptions(t, SplitTunnelState{Enabled: true, Policy: SplitTunnelPolicyInclude,
		Filters: SplitTunnelFilter{ProcessPath: []string{"/apps/browser"}},
	})
	rules := splitPolicyTestRules(t, opts, AutoSelectTag)
	for _, network := range []string{"tcp", "udp"} {
		metadata := A.InboundContext{Network: network, IPVersion: 6,
			Destination: M.Socksaddr{Addr: netip.MustParseAddr("2001:4860:4860::8888"), Port: 443},
		}
		assert.Equal(t, "direct", splitPolicyRoute(rules, metadata))
		metadata.ProcessInfo = &A.ConnectionOwner{ProcessPath: "/apps/browser"}
		want := AutoSelectTag
		if tunHasIPv6(opts) {
			want = C.RuleActionTypeReject
		}
		assert.Equal(t, want, splitPolicyRoute(rules, metadata))
	}
}

func splitPolicyTestOptions(t *testing.T, state SplitTunnelState) O.Options {
	t.Helper()
	dir := t.TempDir()
	st := newSplitTunnel(dir, rlog.NoOpLogger())
	require.NoError(t, st.SetPolicy(state.Policy))
	require.NoError(t, st.AddItems(state.Filters))
	require.NoError(t, st.SetEnabled(state.Enabled))
	cfg := testConfig(t)
	cfg.Options.Route = &O.RouteOptions{
		DefaultDomainResolver: &O.DomainResolveOptions{Server: "dns_remote"},
		Rules: []O.Rule{
			{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{
				RawDefaultRule: O.RawDefaultRule{IPCIDR: []string{"223.5.5.5/32"}},
				RuleAction:     O.RuleAction{Action: C.RuleActionTypeRoute, RouteOptions: O.RouteActionOptions{Outbound: "direct"}},
			}},
			{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{
				RawDefaultRule: O.RawDefaultRule{DomainSuffix: []string{"google.cn"}},
				RuleAction:     O.RuleAction{Action: C.RuleActionTypeRoute, RouteOptions: O.RouteActionOptions{Outbound: "auto"}},
			}},
			{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{
				RawDefaultRule: O.RawDefaultRule{RuleSet: []string{"geosite-cn"}},
				RuleAction:     O.RuleAction{Action: C.RuleActionTypeRoute, RouteOptions: O.RouteActionOptions{Outbound: "direct"}},
			}},
		},
		RuleSet: []O.RuleSet{splitPolicyCountryRuleSet()},
	}
	cfg.Options.DNS = &O.DNSOptions{RawDNSOptions: O.RawDNSOptions{
		Servers: buildDNSServers(), Rules: splitPolicyCloudDNSRules(), Final: "dns_remote",
	}}
	opts, err := buildOptions(BoxOptions{
		BasePath: dir, Options: cfg.Options, SmartRouting: cfg.SmartRouting, AdBlock: cfg.AdBlock, SplitTunnel: state,
	})
	require.NoError(t, err)
	return opts
}

type splitPolicyRouter struct {
	A.Router
	ruleSets map[string]A.RuleSet
}

func (r *splitPolicyRouter) RuleSet(tag string) (A.RuleSet, bool) {
	rs, ok := r.ruleSets[tag]
	return rs, ok
}

type splitPolicyClash struct {
	A.ClashServer
	mode string
}

func (c *splitPolicyClash) Mode() string { return c.mode }

func splitPolicyTestRules(t *testing.T, opts O.Options, mode string) []A.Rule {
	t.Helper()
	ctx := splitPolicyTestContext(t, opts, mode)
	var rules []A.Rule
	for _, ruleOptions := range opts.Route.Rules {
		rule, err := R.NewRule(ctx, log.NewNOPFactory().Logger(), ruleOptions, false)
		require.NoError(t, err)
		require.NoError(t, rule.Start())
		t.Cleanup(func() { assert.NoError(t, rule.Close()) })
		rules = append(rules, rule)
	}
	return rules
}

func splitPolicyCountryRuleSet() O.RuleSet {
	return O.RuleSet{Type: C.RuleSetTypeInline, Tag: "geosite-cn",
		InlineOptions: O.PlainRuleSet{Rules: []O.HeadlessRule{{Type: C.RuleTypeDefault,
			DefaultOptions: O.DefaultHeadlessRule{DomainSuffix: []string{"cn"}}}}},
	}
}

func splitPolicyTestContext(t *testing.T, opts O.Options, mode string) context.Context {
	t.Helper()
	ctx := service.ContextWithDefaultRegistry(context.Background())
	logger := log.NewNOPFactory().Logger()
	router := &splitPolicyRouter{ruleSets: make(map[string]A.RuleSet)}
	service.MustRegister[A.Router](ctx, router)
	service.MustRegister[A.ClashServer](ctx, &splitPolicyClash{mode: mode})
	service.MustRegister(ctx, new(A.NetworkManager))
	for _, ruleSetOptions := range opts.Route.RuleSet {
		if ruleSetOptions.Type == C.RuleSetTypeRemote {
			fixture := map[string][]string{
				"sr-direct": {"ir", "cn"}, "openai": {"ai.test"},
				"adblock-1": {"blocked.test"}, "adblock-2": {"other-blocked.test"},
			}[ruleSetOptions.Tag]
			require.NotEmpty(t, fixture, ruleSetOptions.Tag)
			ruleSetOptions = O.RuleSet{Type: C.RuleSetTypeInline, Tag: ruleSetOptions.Tag,
				InlineOptions: O.PlainRuleSet{Rules: []O.HeadlessRule{{Type: C.RuleTypeDefault,
					DefaultOptions: O.DefaultHeadlessRule{DomainSuffix: fixture}}}},
			}
		}
		rs, err := R.NewRuleSet(ctx, logger, ruleSetOptions)
		require.NoError(t, err)
		require.NoError(t, rs.StartContext(ctx, new(A.HTTPStartContext)))
		t.Cleanup(func() { assert.NoError(t, rs.Close()) })
		router.ruleSets[ruleSetOptions.Tag] = rs
	}
	return ctx
}

func splitPolicyRoute(rules []A.Rule, metadata A.InboundContext) string {
	for _, rule := range rules {
		metadata.ResetRuleCache()
		if !rule.Match(&metadata) {
			continue
		}
		switch action := rule.Action().(type) {
		case *R.RuleActionRoute:
			return action.Outbound
		case *R.RuleActionReject:
			return C.RuleActionTypeReject
		}
	}
	return ""
}
