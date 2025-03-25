package ruleset

import (
	"context"
	"net/netip"
	"os"
	"strings"
	"testing"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager(t *testing.T) {
	rsTag := "rule-set"
	// domain := "ipconfig.io"
	path, err := os.MkdirTemp("", "test")
	require.NoError(t, err)
	defer os.RemoveAll(path)

	ctx := box.Context(context.Background(), include.InboundRegistry(), include.OutboundRegistry(), include.EndpointRegistry())
	instance, err := box.New(box.Options{
		Context: ctx,
		Options: testOptions(rsTag, path),
	})
	require.NoError(t, err)
	err = instance.Start()
	require.NoError(t, err)
	defer instance.Close()

	router := service.FromContext[adapter.Router](ctx)
	rules := router.Rules()
	ruleset, _ := router.RuleSet("split-tunnel")
	t.Logf("ruleset: %+v", ruleset.(*rule.LocalRuleSet))
	idx := common.Index(rules, func(it adapter.Rule) bool {
		return strings.Contains(it.String(), "rule_set=split-tunnel")
	})
	lRule := rules[idx]
	t.Logf("rule: %+v", lRule)
	t.Logf("match: %v", lRule.Match(testInboundCtx))

	// opts := logicalRuleOpts(true).LogicalOptions
	// newRule, err := rule.NewLogicalRule(ctx, log.StdLogger(), opts)
	// require.NoError(t, err)
	//
	// rules[idx] = newRule
	// router = service.FromContext[adapter.Router](ctx)
	// rules = router.Rules()
	// idx = common.Index(rules, func(it adapter.Rule) bool {
	// 	return strings.Contains(it.String(), "rule_set=split-tunnel")
	// })
	// lRule = rules[idx]
	// t.Logf("rule: %v", lRule)
	// t.Logf("match: %v", lRule.Match(testInboundCtx))
}

func TestStart(t *testing.T) {
	rsTag := "rule-set"
	domain := "ipconfig.io"
	path, err := os.MkdirTemp("", "test")
	require.NoError(t, err)
	defer os.RemoveAll(path)
	os.WriteFile(path+"/"+rsTag+".json", []byte(`{"domain":["`+domain+`"]}`), 0644)

	ctx := box.Context(context.Background(), include.InboundRegistry(), include.OutboundRegistry(), include.EndpointRegistry())

	_, err = box.New(box.Options{
		Context: ctx,
		Options: testOptions(rsTag, path),
	})
	require.NoError(t, err)

	m := newMutableRuleSet(path, rsTag, false)
	require.NoError(t, m.Start(ctx), "Start failed")
	require.Len(t, m.rules, 1, "Rules not loaded")

	rule := m.rules[0].(*ruleWrapper)
	require.Equal(t, rule.name, rsTag, "Rule name mismatch")
	require.Contains(t, m.filter.Domain, domain, "Rule not loaded")
}

func TestAddRemoveItems(t *testing.T) {
	path, err := os.MkdirTemp("", "test")
	require.NoError(t, err)
	defer os.RemoveAll(path)

	m := newMutableRuleSet(path, "test", false)

	// test AddItem
	assert.NoError(t, m.AddItem(TypeDomain, "example.com"), "AddItem failed")
	assert.Contains(t, m.filter.Domain, "example.com", "Item not added")

	assert.Error(t, m.AddItem("unsupportedType", "example.com"), "AddItem should have failed")

	// test RemoveItem
	assert.NoError(t, m.RemoveItem(TypeDomain, "example.com"), "RemoveItem failed")
	assert.NotContains(t, m.filter.Domain, "example.com", "Item not removed")

	assert.Error(t, m.RemoveItem("unsupportedType", "example.com"), "RemoveItem should have failed")
}

var testInboundCtx = &adapter.InboundContext{
	IPVersion: 4,
	Domain:    "ipconfig.io",
}

func testOptions(rsTag, rsPath string) option.Options {
	opts := option.Options{
		Log: &option.LogOptions{Disabled: true},
		Inbounds: []option.Inbound{
			{
				Type: constant.TypeHTTP,
				Tag:  "http-in",
				Options: &option.HTTPMixedInboundOptions{
					ListenOptions: option.ListenOptions{
						Listen:     common.Ptr(badoption.Addr(netip.MustParseAddr("127.0.0.1"))),
						ListenPort: 3003,
					},
				},
			},
		},
		Outbounds: []option.Outbound{
			{
				Type: constant.TypeDirect,
			},
			{
				Type: constant.TypeHTTP,
				Tag:  "http-out",
				Options: &option.HTTPOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "127.0.0.1",
						ServerPort: 3000,
					},
				},
			},
		},
		Route: &option.RouteOptions{
			Rules: []option.Rule{
				{
					Type: constant.RuleTypeDefault,
					DefaultOptions: option.DefaultRule{
						RawDefaultRule: option.RawDefaultRule{
							RuleSet: badoption.Listable[string]{rsTag},
						},
						RuleAction: option.RuleAction{
							Action: constant.RuleActionTypeRoute,
							RouteOptions: option.RouteActionOptions{
								Outbound: "http-out",
							},
						},
					},
				},
			},
			RuleSet: []option.RuleSet{
				{
					Type: constant.RuleSetTypeLocal,
					Tag:  rsTag,
					LocalOptions: option.LocalRuleSet{
						Path: rsPath,
						// Rules: []option.HeadlessRule{
						// 	{
						// 		Type: constant.RuleTypeDefault,
						// 		DefaultOptions: option.DefaultHeadlessRule{
						// 			Domain:      []string{domain, "google.com"},
						// 			ProcessName: []string{"test", "test2"},
						// 		},
						// 	},
						// },
					},
				},
			},
		},
	}
	return opts
}
