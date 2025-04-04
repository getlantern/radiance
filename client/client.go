package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/sing-box-extensions/ruleset"

	"github.com/getlantern/radiance/client/boxoptions"
	boxservice "github.com/getlantern/radiance/client/service"
)

var (
	client   *vpnClient
	clientMu sync.Mutex
)

type Options struct {
	DataDir  string
	PlatIfce libbox.PlatformInterface
	// EnableSplitTunneling is the initial state of split tunneling when the service starts
	EnableSplitTunneling bool
}

type VPNClient interface {
	Start() error
	Stop() error
	Pause(dur time.Duration) error
	Resume()
	StartVPN() error
	StopVPN() error
	ConnectionStatus() bool
	PauseVPN(dur time.Duration) error
	ResumeVPN()
	SplitTunnelHandler() *SplitTunnel
	AddCustomServer(tag string, cfg boxservice.ServerConnectConfig) error
	SelectCustomServer(tag string) error
	RemoveCustomServer(tag string) error
}

type vpnClient struct {
	boxService         *boxservice.BoxService
	splitTunnelHandler *SplitTunnel
	started            bool
	connected          bool
}

// NewVPNClient creates a new VPNClient instance if one does not already exist, otherwise returns
// the existing instance. logDir is the path where the log file will be written. logDir can be
// set to "stdout" to write logs to stdout. platIfce is the platform interface used to
// interact with the underlying platform on iOS and Android. On other platforms, it is ignored and
// can be nil.
func NewVPNClient(opts Options, logDir string) (VPNClient, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		return client, nil
	}

	// TODO: We should be fetching the options from the server.
	logOutput := filepath.Join(logDir, "lantern-box.log")
	boxOpts := boxoptions.Options(logOutput)

	logFactory, err := log.New(log.Options{
		Options: *boxOpts.Log,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create log factory: %w", err)
	}

	rsMgr := ruleset.NewManager()
	splitTun, stRule, stRuleset, err := initSplitTunnel(rsMgr, opts.DataDir, opts.EnableSplitTunneling)
	if err != nil {
		return nil, fmt.Errorf("split tunnel handler: %w", err)
	}
	// inject split tunnel routing rule and ruleset into the routing table
	// the split tunnel routing rule needs to be the first rule with the "route" rule action so it's
	// evaluated first. we're assuming the sniff action rule is at index 0, so we're inserting at
	// index 1
	boxOpts.Route = injectRouteRules(boxOpts.Route, 1, []option.Rule{*stRule}, []option.RuleSet{*stRuleset})

	buf, err := json.Marshal(boxOpts)
	if err != nil {
		return nil, err
	}

	b, err := boxservice.New(string(buf), opts.DataDir, opts.PlatIfce, rsMgr)
	if err != nil {
		return nil, err
	}

	client = &vpnClient{
		boxService:         b,
		splitTunnelHandler: splitTun,
	}
	return client, nil
}

// Start starts the VPN client
func (c *vpnClient) StartVPN() error {
	clientMu.Lock()
	defer clientMu.Unlock()
	if c.started {
		return errors.New("VPN client is already running")
	}

	slog.Debug("Starting VPN client")
	if c.boxService == nil {
		return errors.New("box service is not initialized")
	}
	err := c.boxService.Start()
	if err != nil {
		return err
	}

	c.started = true
	return nil
}

// Stop stops the VPN client and closes the TUN device
func (c *vpnClient) StopVPN() error {
	clientMu.Lock()
	defer clientMu.Unlock()
	if !c.started {
		return errors.New("VPN client is not running")
	}

	slog.Debug("Stopping VPN client")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	var err error
	go func() {
		err = c.boxService.Close()
		cancel()
	}()
	<-ctx.Done()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("box did not stop in time")
	}
	return err
}

// ConnectionStatus returns the connection status of the VPN client
func (c *vpnClient) ConnectionStatus() bool {
	clientMu.Lock()
	defer clientMu.Unlock()
	return c.started && c.connected
}

func (c *vpnClient) setConnectionStatus(connected bool) {
	clientMu.Lock()
	defer clientMu.Unlock()
	c.connected = connected
}

func parseConfig(ctx context.Context, configContent string) (option.Options, error) {
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(configContent))
	if err != nil {
		return option.Options{}, fmt.Errorf("unmarshal config: %w", err)
	}
	return options, nil
}

// Pause pauses the VPN client for the specified duration
func (c *vpnClient) PauseVPN(dur time.Duration) error {
	slog.Info("Pausing VPN for", "duration", dur)
	return c.boxService.Pause(dur)
}

// Resume resumes the VPN client
func (c *vpnClient) ResumeVPN() {
	slog.Info("Resuming VPN client")
	c.boxService.Wake()
}

func (c *vpnClient) AddCustomServer(tag string, cfg boxservice.ServerConnectConfig) error {
	return c.boxService.AddCustomServer(tag, cfg)
}

func (c *vpnClient) SelectCustomServer(tag string) error {
	return c.boxService.SelectCustomServer(tag)
}

func (c *vpnClient) RemoveCustomServer(tag string) error {
	return c.boxService.RemoveCustomServer(tag)
}

func (c *vpnClient) SplitTunnelHandler() *SplitTunnel {
	return c.splitTunnelHandler
}

const (
	SplitTunnelTag    = "split-tunnel"
	SplitTunnelFormat = constant.RuleSetFormatSource // file will be saved as json
)

type SplitTunnel = ruleset.MutableRuleSet

// initSplitTunnel initializes the split tunnel ruleset handler. It retrieves an existing mutable
// ruleset associated with the SplitTunnelTag or creates a new one if it doesn't exist. dataDir is
// the directory where the ruleset data is stored. The initial state is determined by the enabled
// parameter.
func initSplitTunnel(mgr *ruleset.Manager, dataDir string, enabled bool) (*SplitTunnel, *option.Rule, *option.RuleSet, error) {
	rs := mgr.MutableRuleSet(SplitTunnelTag)
	if rs == nil {
		var err error
		rs, err = mgr.NewMutableRuleSet(dataDir, SplitTunnelTag, SplitTunnelFormat, enabled)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	rRule := ruleset.BaseRouteRule(SplitTunnelTag, "direct")
	rRuleset := ruleset.LocalRuleSet(SplitTunnelTag, rs.RuleFilePath(), SplitTunnelFormat)
	return (*SplitTunnel)(rs), &rRule, &rRuleset, nil
}

// injectRouteRules injects the given rules and rulesets into routeOpts. atIdx specifies the index
// at which to insert the rules. rulesets are just appended to the end of the ruleset list as their
// order doesn't matter.
func injectRouteRules(routeOpts *option.RouteOptions, atIdx int, rules []option.Rule, rulesets []option.RuleSet) *option.RouteOptions {
	if atIdx > len(routeOpts.Rules) {
		atIdx = len(routeOpts.Rules)
	}
	routeOpts.Rules = slices.Insert(routeOpts.Rules, atIdx, rules...)
	if routeOpts.RuleSet == nil {
		routeOpts.RuleSet = rulesets
	} else {
		routeOpts.RuleSet = append(routeOpts.RuleSet, rulesets...)
	}
	return routeOpts
}
