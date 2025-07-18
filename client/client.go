package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	"github.com/getlantern/sing-box-extensions/ruleset"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/client/boxoptions"
	boxservice "github.com/getlantern/radiance/client/service"
	"github.com/getlantern/radiance/common"
)

var (
	client   *vpnClient
	clientMu sync.Mutex
	statusMu sync.Mutex

	log = slog.Default()
)

type Options struct {
	DataDir  string
	LogDir   string
	PlatIfce libbox.PlatformInterface
	// EnableSplitTunneling is the initial state of split tunneling when the service starts
	EnableSplitTunneling bool
}

type VPNClient interface {
	StartVPN() error
	StopVPN() error
	ConnectionStatus() bool
	PauseVPN(dur time.Duration) error
	ResumeVPN()
	SplitTunnelHandler() *SplitTunnel
	SelectServer(group, tag string) error
	AddCustomServer(cfg boxservice.ServerConnectConfig) error
	RemoveCustomServer(tag string) error
	// Lantern Server Manager Integration

	AddServerManagerInstance(tag string, ip string, port int, accessToken string, callback boxservice.TrustFingerprintCallback) error
	InviteToServerManagerInstance(ip string, port int, accessToken string, inviteName string) (string, error)
	RevokeServerManagerInvite(ip string, port int, accessToken string, inviteName string) error
}

type vpnClient struct {
	boxService          *boxservice.BoxService
	splitTunnelHandler  *SplitTunnel
	customServerManager *boxservice.CustomServerManager
	running             atomic.Bool
	connected           bool
}

// NewVPNClient creates a new VPNClient instance if one does not already exist, otherwise returns
// the existing instance. The client will be initialized with the provided [libbox.PlatformInterface]
// if has not been initialized yet, or will return the existing client instance. platIfce used to
// interact with the underlying platform on iOS and Android. On other platforms, it is ignored and
// can be nil. enableSplitTunnel is the initial state of split tunneling when the service starts.
func NewVPNClient(dataDir, logDir string, platIfce libbox.PlatformInterface, enableSplitTunnel bool) (VPNClient, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		return client, nil
	}

	// TODO: accept log level as an argument
	if err := common.Init(dataDir, logDir, "debug"); err != nil {
		return nil, fmt.Errorf("failed to initialize: %w", err)
	}
	log = slog.Default().With("name", "vpn-client")
	dataDir = common.DataPath()

	boxOpts := boxoptions.Options()

	log.Debug("Creating new VPN client")
	rsMgr := ruleset.NewManager()
	splitTunnel, err := initMutRuleSet(dataDir, SplitTunnelTag, SplitTunnelFormat, rsMgr, enableSplitTunnel)
	if err != nil {
		return nil, fmt.Errorf("split tunnel handler: %w", err)
	}
	customServerSelector, err := initMutRuleSet(
		dataDir,
		CustomSelectorTag,
		CustomSelectorFormat,
		rsMgr,
		true, // TODO: maybe this should be saved and restored to remember the user's last choice
	)
	if err != nil {
		return nil, fmt.Errorf("customServerSelector ruleset: %w", err)
	}

	// inject split tunnel routing rule and ruleset into the routing table
	// the split tunnel routing rule needs to be the first rule with the "route" rule action so it's
	// evaluated first. we're assuming the sniff action rule is at index 0, so we're inserting at
	// index 1
	boxOpts.Route = injectRouteRules(
		boxOpts.Route, 1,
		[]option.Rule{splitTunnel.ruleOption, customServerSelector.ruleOption},
		[]option.RuleSet{splitTunnel.rulesetOption, customServerSelector.rulesetOption},
	)

	buf, err := json.Marshal(boxOpts)
	if err != nil {
		return nil, err
	}

	userSM, err := boxservice.NewCustomServerManager(dataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create custom server manager: %w", err)
	}
	b, err := boxservice.New(string(buf), app.ConfigFileName, platIfce, rsMgr, log, userSM)
	if err != nil {
		return nil, err
	}

	client = &vpnClient{
		boxService:          b,
		customServerManager: userSM,
		splitTunnelHandler:  splitTunnel.mutableRuleSet,
	}
	return client, nil
}

// StartVPN Start starts the VPN client
func (c *vpnClient) StartVPN() error {
	if c.running.Load() {
		return errors.New("VPN client is already running")
	}

	clientMu.Lock()
	defer clientMu.Unlock()

	log.Debug("Starting VPN client")
	if c.boxService == nil {
		return errors.New("box service is not initialized")
	}
	err := c.boxService.Start()
	if err != nil {
		log.Error("Failed to start boxService", "error", err)
		return err
	}

	c.running.Store(true)
	c.setConnectionStatus(true)
	return nil
}

// StopVPN Stop stops the VPN client and closes the TUN device
func (c *vpnClient) StopVPN() error {
	if !c.running.Load() {
		return errors.New("VPN client is not running")
	}

	clientMu.Lock()
	defer clientMu.Unlock()

	log.Debug("Stopping VPN client")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	var err error
	go func() {
		err = c.boxService.Close()
		cancel()
		c.running.Store(false)
	}()
	<-ctx.Done()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("box did not stop in time")
	}
	c.running.Store(false)
	c.setConnectionStatus(false)
	return err
}

// ConnectionStatus returns the connection status of the VPN client
func (c *vpnClient) ConnectionStatus() bool {
	clientMu.Lock()
	defer clientMu.Unlock()
	return c.running.Load() && c.connected
}

func (c *vpnClient) setConnectionStatus(connected bool) {
	statusMu.Lock()
	defer statusMu.Unlock()
	c.connected = connected
}

// PauseVPN Pause pauses the VPN client for the specified duration
func (c *vpnClient) PauseVPN(dur time.Duration) error {
	log.Info("Pausing VPN for", "duration", dur)
	return c.boxService.Pause(dur)
}

// ResumeVPN Resume resumes the VPN client
func (c *vpnClient) ResumeVPN() {
	log.Info("Resuming VPN client")
	c.boxService.Wake()
}

// Lantern Server Manager Integration

// AddServerManagerInstance will fetch VPN connection information from the server manager instance and add it to the VPN client as a custom server
// The server manager instance is identified by the tag, ip, port and accessToken.
// The accessToken is used to authenticate the connection to the server manager instance.
// The callback is used to verify the server manager instance's certificate fingerprint.
// If we don't have the fingerprint, we will use the default callback which will ask the user to trust the fingerprint.
func (c *vpnClient) AddServerManagerInstance(tag string, ip string, port int, accessToken string, callback boxservice.TrustFingerprintCallback) error {
	return c.customServerManager.AddServerManagerInstance(tag, ip, port, accessToken, callback)
}

// InviteToServerManagerInstance will invite another user (identified by inviteName) to the server manager instance and return the token that can be used to connect to the server manager instance
// The server must be added to the VPN client as a custom server first and have a trusted fingerprint.
func (c *vpnClient) InviteToServerManagerInstance(ip string, port int, accessToken string, inviteName string) (string, error) {
	return c.customServerManager.InviteToServerManagerInstance(ip, port, accessToken, inviteName)
}

// RevokeServerManagerInvite will revoke an invite to the server manager instance
// The server must be added to the VPN client as a custom server first and have a trusted fingerprint.
func (c *vpnClient) RevokeServerManagerInvite(ip string, port int, accessToken string, inviteName string) error {
	return c.customServerManager.RevokeServerManagerInvite(ip, port, accessToken, inviteName)
}

// ActiveServer returns the current connected server as a [boxservice.Server].
func (c *vpnClient) ActiveServer() (*boxservice.Server, error) {
	if !c.ConnectionStatus() {
		return nil, fmt.Errorf("VPN is not connected")
	}
	activeServer, err := c.boxService.ActiveServer()
	if err != nil {
		return nil, fmt.Errorf("get active server: %w", err)
	}
	return &activeServer, nil
}

// SelectServer selects a server by its tag and group. Valid groups are [boxoptions.ServerGroupUser]
// and [boxoptions.ServerGroupLantern]. An error is returned if a server config with the given group
// and tag is not found.
// SelectServer DOES NOT start the service, it only sets the server to connect to when the service
// is started. If the service is already running and the selected server is valid, it will connect to
// the server immediately.
func (c *vpnClient) SelectServer(group, tag string) error {
	return c.boxService.SelectServer(group, tag)
}

// AddCustomServer adds a user-defined server to the VPN client.
// Note, if the service is running, it must be stopped before the new server can be selected.
func (c *vpnClient) AddCustomServer(cfg boxservice.ServerConnectConfig) error {
	return c.customServerManager.AddCustomServer(cfg)
}

func (c *vpnClient) RemoveCustomServer(tag string) error {
	return c.boxService.RemoveUserServer(tag)
}

func (c *vpnClient) SplitTunnelHandler() *SplitTunnel {
	return c.splitTunnelHandler
}

const (
	SplitTunnelTag       = "split-tunnel"
	SplitTunnelFormat    = constant.RuleSetFormatSource // file will be saved as json
	CustomSelectorTag    = "custom-server"
	CustomSelectorFormat = constant.RuleSetFormatSource // file will be saved as json
)

type SplitTunnel = ruleset.MutableRuleSet
type CustomServer = ruleset.MutableRuleSet

type tunnel struct {
	mutableRuleSet *ruleset.MutableRuleSet
	ruleOption     option.Rule
	rulesetOption  option.RuleSet
}

// initMutRuleSet initializes the ruleset handler. It retrieves an existing mutable
// ruleset associated with the provided tag or cerates a new one if it doesn't
// exist. dataDir is the directory where the ruleset data is stored. The initial
// state is determined by the enabled parameter.
func initMutRuleSet(dataDir, tag, format string, mgr *ruleset.Manager, enabled bool) (tunnel, error) {
	rs := mgr.MutableRuleSet(tag)
	if rs == nil {
		var err error
		rs, err = mgr.NewMutableRuleSet(dataDir, tag, format, enabled)
		if err != nil {
			return tunnel{}, err
		}
	}
	return tunnel{
		mutableRuleSet: rs,
		ruleOption:     ruleset.BaseRouteRule(tag, "direct"),
		rulesetOption:  ruleset.LocalRuleSet(tag, rs.RuleFilePath(), format),
	}, nil
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
