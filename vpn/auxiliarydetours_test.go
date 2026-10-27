package vpn

import (
	"testing"

	lcommon "github.com/getlantern/common"
	box "github.com/getlantern/lantern-box"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOptionsValidatesAuxiliaryDetours(t *testing.T) {
	for _, kind := range []string{"udp", "tls", "https", "legacy", "rule-set", "smart-rule-set"} {
		for _, detour := range []string{"", "working", "direct", "sr-live", "via-unbounded", "missing"} {
			for _, source := range []string{"fresh", "cached"} {
				t.Run(kind+"/"+detour+"/"+source, func(t *testing.T) {
					input := BoxOptions{
						BasePath: t.TempDir(),
						Options:  O.Options{Outbounds: detourOutbounds()},
						SmartRouting: lcommon.SmartRoutingRules{
							{Category: "live", Outbounds: []string{"working"}},
						},
					}
					remote := O.RemoteDNSServerOptions{
						RawLocalDNSServerOptions: O.RawLocalDNSServerOptions{DialerOptions: O.DialerOptions{Detour: detour}},
						DNSServerAddressOptions:  O.DNSServerAddressOptions{Server: "192.0.2.1"},
					}
					server := O.DNSServerOptions{Type: kind, Tag: "resolver"}
					switch kind {
					case "udp":
						server.Options = remote
					case "tls":
						server.Options = &O.RemoteTLSDNSServerOptions{RemoteDNSServerOptions: remote}
					case "https":
						server.Options = &O.RemoteHTTPSDNSServerOptions{
							RemoteTLSDNSServerOptions: O.RemoteTLSDNSServerOptions{RemoteDNSServerOptions: remote},
						}
					case "legacy":
						server.Type = C.DNSTypeLegacy
						server.Options = &O.LegacyDNSServerOptions{Address: "tcp://192.0.2.1", Detour: detour}
					case "rule-set":
						input.Options.Route = &O.RouteOptions{RuleSet: []O.RuleSet{{
							Type: C.RuleSetTypeRemote, Tag: "remote", Format: C.RuleSetFormatSource,
							RemoteOptions: O.RemoteRuleSet{URL: "https://rules.example/test.json", DownloadDetour: detour},
						}}}
					case "smart-rule-set":
						input.SmartRouting[0].RuleSets = lcommon.RuleSets{{
							Tag: "remote", URL: "https://rules.example/test.json", DownloadDetour: detour,
						}}
					}
					if server.Options != nil {
						input.Options.DNS = &O.DNSOptions{RawDNSOptions: O.RawDNSOptions{
							Servers: []O.DNSServerOptions{server},
						}}
					}
					ctx := box.Context(t.Context())
					if source == "cached" {
						encoded, err := json.MarshalContext(ctx, input)
						require.NoError(t, err)
						input, err = json.UnmarshalExtendedContext[BoxOptions](ctx, encoded)
						require.NoError(t, err)
					}
					original, err := json.MarshalContext(ctx, input)
					require.NoError(t, err)
					options, err := buildOptions(input)
					if detour == "via-unbounded" || detour == "missing" {
						require.ErrorContains(t, err, `detour "`+detour+`" is unavailable`)
						assert.Contains(t, err.Error(), "restore the outbound")
						if server.Options != nil {
							assert.Contains(t, err.Error(), `DNS server "resolver"`)
						} else {
							assert.Contains(t, err.Error(), `rule set "remote"`)
						}
					} else {
						require.NoError(t, err)
						assert.NotNil(t, findOutbound(options.Outbounds, "working"))
						assert.NotNil(t, findOutbound(options.Outbounds, "sr-live"))
					}
					after, err := json.MarshalContext(ctx, input)
					require.NoError(t, err)
					assert.JSONEq(t, string(original), string(after))
				})
			}
		}
	}
}
