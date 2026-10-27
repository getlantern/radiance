package vpn

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	box "github.com/getlantern/lantern-box"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	R "github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRejectMissingOutboundRoutes(t *testing.T) {
	rules := []O.Rule{
		{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{
			RawDefaultRule: O.RawDefaultRule{Domain: []string{"blocked.example"}},
			RuleAction:     O.RuleAction{RouteOptions: O.RouteActionOptions{Outbound: "missing"}},
		}},
		{Type: C.RuleTypeLogical, LogicalOptions: O.LogicalRule{
			RawLogicalRule: O.RawLogicalRule{Mode: C.LogicalTypeAnd, Invert: true, Rules: []O.Rule{
				{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{RawDefaultRule: O.RawDefaultRule{Port: []uint16{443}}}},
			}},
			RuleAction: O.RuleAction{Action: C.RuleActionTypeRoute, RouteOptions: O.RouteActionOptions{Outbound: "missing"}},
		}},
		{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{
			RuleAction: O.RuleAction{Action: C.RuleActionTypeRoute, RouteOptions: O.RouteActionOptions{Outbound: "working"}},
		}},
		{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{
			RuleAction: O.RuleAction{Action: C.RuleActionTypeSniff},
		}},
	}
	got := rejectMissingOutboundRoutes(rules, map[string]bool{"working": true})
	require.Len(t, got, len(rules))
	assert.Equal(t, rules[0].DefaultOptions.RawDefaultRule, got[0].DefaultOptions.RawDefaultRule)
	assert.Equal(t, rules[1].LogicalOptions.RawLogicalRule, got[1].LogicalOptions.RawLogicalRule)
	assert.Equal(t, O.RuleAction{Action: C.RuleActionTypeReject}, got[0].DefaultOptions.RuleAction)
	assert.Equal(t, O.RuleAction{Action: C.RuleActionTypeReject}, got[1].LogicalOptions.RuleAction)
	assert.Equal(t, rules[2:], got[2:])
	assert.Equal(t, "missing", rules[0].DefaultOptions.RouteOptions.Outbound)
	assert.Equal(t, "missing", rules[1].LogicalOptions.RouteOptions.Outbound)
}

func TestRejectMissingBypassRoutes(t *testing.T) {
	for _, target := range []string{"missing", "working", ""} {
		t.Run(target, func(t *testing.T) {
			action := O.RuleAction{Action: C.RuleActionTypeBypass, BypassOptions: O.RouteActionOptions{Outbound: target}}
			rules := []O.Rule{
				{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{
					RawDefaultRule: O.RawDefaultRule{Domain: []string{"blocked.example"}},
					RuleAction:     action,
				}},
				{Type: C.RuleTypeLogical, LogicalOptions: O.LogicalRule{
					RawLogicalRule: O.RawLogicalRule{Mode: C.LogicalTypeAnd, Invert: true, Rules: []O.Rule{
						{Type: C.RuleTypeDefault, DefaultOptions: O.DefaultRule{RawDefaultRule: O.RawDefaultRule{Port: []uint16{443}}}},
					}},
					RuleAction: action,
				}},
			}
			got := rejectMissingOutboundRoutes(rules, map[string]bool{"working": true})
			require.Len(t, got, len(rules))
			assert.Equal(t, rules[0].DefaultOptions.RawDefaultRule, got[0].DefaultOptions.RawDefaultRule)
			assert.Equal(t, rules[1].LogicalOptions.RawLogicalRule, got[1].LogicalOptions.RawLogicalRule)
			want := action
			if target == "missing" {
				want = O.RuleAction{Action: C.RuleActionTypeReject}
			}
			assert.Equal(t, want, got[0].DefaultOptions.RuleAction)
			assert.Equal(t, want, got[1].LogicalOptions.RuleAction)
			assert.Equal(t, action, rules[0].DefaultOptions.RuleAction)
			assert.Equal(t, action, rules[1].LogicalOptions.RuleAction)
		})
	}
}

func TestTunnelRejectsRoutesToFilteredGroups(t *testing.T) {
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(probe.Close)
	for _, source := range []string{"fresh", "cached"} {
		t.Run(source, func(t *testing.T) {
			input, err := json.UnmarshalExtendedContext[BoxOptions](box.Context(t.Context()), []byte(`{
				"options": {
					"outbounds": [
						{"type": "reflex", "tag": "reflex"},
						{"type": "selector", "tag": "empty", "outbounds": ["reflex"]},
						{"type": "direct", "tag": "working"}
					],
					"route": {"rules": [
						{"domain": "blocked.example", "outbound": "empty"},
						{"type": "logical", "mode": "and", "rules": [
							{"domain": "logical.example"}, {"port": 443}
						], "outbound": "empty"},
						{"domain": "bypass.example", "action": "bypass", "outbound": "empty"},
						{"type": "logical", "mode": "and", "rules": [
							{"domain": "logical-bypass.example"}, {"port": 443}
						], "action": "bypass", "outbound": "empty"},
						{"domain": "working.example", "outbound": "working"}
					]}
				}
			}`))
			require.NoError(t, err)
			input.BasePath = t.TempDir()
			original, err := json.MarshalContext(box.Context(t.Context()), input)
			require.NoError(t, err)
			if source == "cached" {
				input, err = json.UnmarshalExtendedContext[BoxOptions](box.Context(t.Context()), original)
				require.NoError(t, err)
			}
			options, err := buildOptions(input)
			require.NoError(t, err)
			after, err := json.MarshalContext(box.Context(t.Context()), input)
			require.NoError(t, err)
			assert.JSONEq(t, string(original), string(after))
			options.Inbounds = nil
			options.Log = &O.LogOptions{Disabled: true}
			findOutbound(options.Outbounds, AutoSelectTag).Options.(*lbO.MutableAutoSelectOutboundOptions).URL = probe.URL
			tun := &tunnel{dataPath: input.BasePath}
			t.Cleanup(func() { assert.NoError(t, tun.close()) })
			require.NoError(t, tun.init(t.Context(), options, nil))
			require.NoError(t, tun.boxInstance.Start())
			for _, domain := range []string{"blocked.example", "logical.example", "bypass.example", "logical-bypass.example"} {
				_, err := tun.boxInstance.Router().PreMatch(adapter.InboundContext{
					Network: "tcp", Domain: domain, Destination: M.Socksaddr{Fqdn: domain, Port: 443},
				}, nil, time.Second, false)
				assert.True(t, R.IsRejected(err), "%s should be rejected, got %v", domain, err)
			}
			_, err = tun.boxInstance.Router().PreMatch(adapter.InboundContext{
				Network: "tcp", Domain: "working.example", Destination: M.Socksaddr{Fqdn: "working.example", Port: 443},
			}, nil, time.Second, false)
			assert.NoError(t, err)
		})
	}
}
