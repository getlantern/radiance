package splittunnel

import (
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
)

type SplitTunnelOptions struct {
	Enabled  bool   `json:"enabled,omitempty"`
	DataPath string `json:"data_path,omitempty"`
}

type splitTunnelRule struct {
	adapter.Rule
	enabled *atomic.Bool
}

func (s *splitTunnelRule) Match(metadata *adapter.InboundContext) bool {
	return s.enabled.Load() && s.Rule.Match(metadata)
}

func (s *splitTunnelRule) Type() string {
	return s.Rule.Type() + "-split-tunnel"
}

func RouteRule() option.Rule {
	return option.Rule{
		Type: constant.RuleTypeDefault,
		DefaultOptions: option.DefaultRule{
			RawDefaultRule: option.RawDefaultRule{
				RuleSet: badoption.Listable[string]{"split-tunnel"},
			},
			RuleAction: option.RuleAction{
				Action: constant.RuleActionTypeRoute,
				RouteOptions: option.RouteActionOptions{
					Outbound: "direct",
				},
			},
		},
	}
}
