package mutruleset

import (
	"github.com/getlantern/sing-box-extensions/ruleset"
	"github.com/sagernet/sing-box/constant"
)

const (
	SplitTunnelTag    = "split-tunnel"
	SplitTunnelFormat = constant.RuleSetFormatSource // file will be saved as json
)

type SplitTunnel = ruleset.MutableRuleSet

// SplitTunnelHandler initializes the split tunnel ruleset handler. It retrieves an existing mutable
// ruleset associated with the SplitTunnelTag or creates a new one if it doesn't exist. dataDir is
// the directory where the ruleset data is stored. The initial state is determined by the enabled
// parameter.
func SplitTunnelHandler(mgr *ruleset.Manager, dataDir string, enabled bool) (*SplitTunnel, error) {
	rs := mgr.MutableRuleSet(SplitTunnelTag)
	if rs == nil {
		var err error
		rs, err = mgr.NewMutableRuleSet(dataDir, SplitTunnelTag, SplitTunnelFormat, enabled)
		if err != nil {
			return nil, err
		}
	}
	return (*SplitTunnel)(rs), nil
}
