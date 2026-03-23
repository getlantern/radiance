// Package backend provides the main interface for all the major components of Radiance.
package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Xuanwo/go-locale"
	"github.com/sagernet/sing-box/option"
	"go.opentelemetry.io/otel"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/deviceid"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/issue"
	"github.com/getlantern/radiance/kindling"
	"github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/telemetry"
	"github.com/getlantern/radiance/traces"
	"github.com/getlantern/radiance/vpn"
)

const tracerName = "github.com/getlantern/backend"

// LocalBackend ties all the core functionality of Radiance together. It manages the configuration,
// servers, VPN connection, account management, issue reporting, and telemetry for the application.
type LocalBackend struct {
	ctx           context.Context
	confHandler   *config.ConfigHandler
	issueReporter *issue.IssueReporter
	accountClient *account.Client

	srvManager     *servers.Manager
	vpnClient      *vpn.VPNClient
	splitTunnelMgr *vpn.SplitTunnel

	shutdownFuncs []func() error
	closeOnce     sync.Once
	stopChan      chan struct{}

	deviceID string

	telemetryCfgSub atomic.Pointer[events.Subscription[config.NewConfigEvent]]
	stopConnMetrics func()
	connMetricsMu   sync.Mutex
	vpnStatusSub    *events.Subscription[vpn.StatusUpdateEvent]
}

type Options struct {
	DataDir  string
	LogDir   string
	Locale   string
	LogLevel string
	// this should be the platform device ID on mobile devices, desktop platforms will generate their
	// own device ID and ignore this value
	DeviceID string
	// User choice for telemetry consent
	TelemetryConsent  bool
	PlatformInterface vpn.PlatformInterface
}

// NewLocalBackend performs global initialization and returns a new LocalBackend instance.
// It should be called once at the start of the application.
func NewLocalBackend(ctx context.Context, opts Options) (*LocalBackend, error) {
	if err := common.Init(opts.DataDir, opts.LogDir, opts.LogLevel); err != nil {
		return nil, fmt.Errorf("failed to initialize common components: %w", err)
	}
	if opts.Locale == "" {
		if tag, err := locale.Detect(); err != nil {
			opts.Locale = "en-US"
		} else {
			opts.Locale = tag.String()
		}
	}

	var platformDeviceID string
	switch common.Platform {
	case "ios", "android":
		platformDeviceID = opts.DeviceID
	default:
		platformDeviceID = deviceid.Get()
	}

	dataDir := settings.GetString(settings.DataPathKey)
	disableFetch := env.GetBool(env.DisableFetch)
	settings.Patch(settings.Settings{
		settings.LocaleKey:              opts.Locale,
		settings.DeviceIDKey:            platformDeviceID,
		settings.ConfigFetchDisabledKey: disableFetch,
		settings.TelemetryKey:           opts.TelemetryConsent,
	})

	kindling.SetKindling(kindling.NewKindling(dataDir))
	accountClient := account.NewClient(kindling.HTTPClient(), dataDir)

	svrMgr, err := servers.NewManager(
		dataDir, slog.Default().With("service", "server_manager"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create server manager: %w", err)
	}

	splitTunnelMgr, err := vpn.NewSplitTunnelHandler(
		dataDir, slog.Default().With("service", "split_tunnel"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create split tunnel manager: %w", err)
	}

	cOpts := config.Options{
		DataPath:      dataDir,
		Locale:        opts.Locale,
		AccountClient: accountClient,
		HTTPClient:    kindling.HTTPClient(),
		Logger:        slog.Default().With("service", "config_handler"),
	}
	if disableFetch {
		cOpts.PollInterval = -1
		slog.Info("Config fetch disabled via environment variable", "env_var", env.DisableFetch)
	}

	vpnClient := vpn.NewVPNClient(dataDir, slog.Default().With("service", "vpn"), opts.PlatformInterface)
	r := &LocalBackend{
		ctx:            ctx,
		issueReporter:  issue.NewIssueReporter(kindling.HTTPClient()),
		accountClient:  accountClient,
		confHandler:    config.NewConfigHandler(ctx, cOpts),
		srvManager:     svrMgr,
		vpnClient:      vpnClient,
		splitTunnelMgr: splitTunnelMgr,
		shutdownFuncs: []func() error{
			telemetry.Close, kindling.Close, vpnClient.Close,
		},
		stopChan:  make(chan struct{}),
		closeOnce: sync.Once{},
		deviceID:  platformDeviceID,
	}
	return r, nil
}

func (r *LocalBackend) Start() {
	// set country code in settings when new config is received so it can be included in issue reports
	events.SubscribeOnce(func(evt config.NewConfigEvent) {
		if evt.New != nil && evt.New.Country != "" {
			if err := settings.Set(settings.CountryCodeKey, evt.New.Country); err != nil {
				slog.Error("failed to set country code in settings", "error", err)
			}
			slog.Info("Set country code from config response", "country_code", evt.New.Country)
		}
	})
	// update VPN outbounds when new config is received
	events.Subscribe(func(evt config.NewConfigEvent) {
		if evt.New == nil {
			return
		}
		cfg := evt.New
		locs := make(map[string]C.ServerLocation, len(cfg.OutboundLocations))
		for k, v := range cfg.OutboundLocations {
			if v != nil {
				locs[k] = *v
			}
		}
		opts := servers.Options{
			Outbounds: cfg.Options.Outbounds,
			Endpoints: cfg.Options.Endpoints,
			Locations: locs,
		}
		if err := r.setServers(servers.SGLantern, opts); err != nil {
			slog.Error("setting servers in manager", "error", err)
		}
	})
	r.confHandler.Start()
}

// addShutdownFunc adds a shutdown function(s) to the Radiance instance.
// This function is called when the Radiance instance is closed to ensure that all
// resources are cleaned up properly.
func (r *LocalBackend) addShutdownFunc(fns ...func() error) {
	for _, fn := range fns {
		if fn != nil {
			r.shutdownFuncs = append(r.shutdownFuncs, fn)
		}
	}
}

func (r *LocalBackend) Close() {
	r.closeOnce.Do(func() {
		slog.Debug("Closing Radiance")
		r.confHandler.Stop()
		close(r.stopChan)
		for _, shutdown := range r.shutdownFuncs {
			if err := shutdown(); err != nil {
				slog.Error("Failed to shutdown", "error", err)
			}
		}
	})
	<-r.stopChan
}

//////////////////
// Issue Report //
//////////////////

// ReportIssue allows the user to report an issue with the application. It collects relevant
// information about the user's environment such as country, device ID, user ID, subscription level,
// and locale, and log files to include in the report. The additionalAttachments parameter allows
// the caller to include any extra files they want to attach to the issue report.
func (r *LocalBackend) ReportIssue(issueType issue.IssueType, description, email string, additionalAttachments []string) error {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "report_issue")
	defer span.End()
	// get country from the config returned by the backend
	var country string
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		slog.Warn("Failed to get config", "error", err)
	} else {
		country = cfg.Country
	}

	report := issue.IssueReport{
		Type:                  issueType,
		Description:           description,
		Email:                 email,
		CountryCode:           country,
		DeviceID:              r.deviceID,
		UserID:                settings.GetString(settings.UserIDKey),
		SubscriptionLevel:     settings.GetString(settings.UserLevelKey),
		Locale:                settings.GetString(settings.LocaleKey),
		AdditionalAttachments: append(baseIssueAttachments(), additionalAttachments...),
	}
	err = r.issueReporter.Report(ctx, report)
	if err != nil {
		slog.Error("Failed to report issue", "error", err)
		return traces.RecordError(ctx, fmt.Errorf("failed to report issue: %w", err))
	}
	slog.Info("Issue reported successfully")
	return nil
}

// baseIssueAttachments returns a list of file paths to include as attachments in every issue report
// in order of importance.
func baseIssueAttachments() []string {
	logPath := settings.GetString(settings.LogPathKey)
	dataPath := settings.GetString(settings.DataPathKey)
	// TODO: any other files we want to include?? split-tunnel config?
	return []string{
		filepath.Join(logPath, internal.CrashLogFileName),
		filepath.Join(dataPath, internal.ConfigFileName),
		filepath.Join(dataPath, internal.ServersFileName),
		filepath.Join(dataPath, internal.DebugBoxOptionsFileName),
	}
}

/////////////////
//  Settings   //
/////////////////

// Features returns the features available in the current configuration, returned from the server in the
// config response.
func (r *LocalBackend) Features() map[string]bool {
	_, span := otel.Tracer(tracerName).Start(context.Background(), "features")
	defer span.End()
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		slog.Info("Failed to get config for features", "error", err)
		return map[string]bool{}
	}
	if cfg == nil {
		slog.Info("No config available for features, returning empty map")
		return map[string]bool{}
	}
	slog.Debug("Returning features from config", "features", cfg.Features)
	// Return the features from the config
	if cfg.Features == nil {
		slog.Info("No features available in config, returning empty map")
		return map[string]bool{}
	}
	return cfg.Features
}

func (r *LocalBackend) PatchSettings(updates settings.Settings) error {
	curr := settings.GetAllFor(slices.Collect(maps.Keys(updates))...)
	diff := updates.Diff(curr)
	slog.Log(nil, log.LevelTrace, "Patching settings", "updates", updates, "current", curr, "diff", diff)
	if len(diff) == 0 {
		return nil
	}
	if err := settings.Patch(diff); err != nil {
		return fmt.Errorf("failed to update settings: %w", err)
	}
	// telemetry settings
	if _, ok := diff[settings.TelemetryKey]; ok {
		if settings.GetBool(settings.TelemetryKey) {
			if err := r.startTelemetry(); err != nil {
				slog.Error("Failed to start telemetry", "error", err)
			}
		} else {
			r.stopTelemetry()
		}
	}

	// vpn settings
	k := settings.SplitTunnelKey
	if _, ok := diff[k]; ok {
		r.splitTunnelMgr.SetEnabled(settings.GetBool(k))
	}
	r.maybeRestartVPN(diff)

	return nil
}

// maybeRestartVPN restarts the VPN connection if either the ad block or smart routing settings
// were changed and the VPN is currently connected.
func (r *LocalBackend) maybeRestartVPN(updates settings.Settings) {
	_, adBlockChanged := updates[settings.AdBlockKey]
	_, smartRoutingChanged := updates[settings.SmartRoutingKey]
	if (adBlockChanged || smartRoutingChanged) && r.vpnClient.Status() == vpn.Connected {
		bOptions := r.getBoxOptions()
		go r.vpnClient.Restart(bOptions)
	}
}

/////////////////
//  telemetry  //
/////////////////

func (r *LocalBackend) startTelemetry() error {
	cfg, err := r.confHandler.GetConfig()
	if err == nil {
		if err := telemetry.Initialize(r.deviceID, *cfg, settings.IsPro()); err != nil {
			return fmt.Errorf("failed to initialize telemetry: %w", err)
		}
	}
	if r.telemetryCfgSub.Load() != nil {
		return nil
	}
	// subscribe to config changes to update telemetry config
	sub := events.Subscribe(func(evt config.NewConfigEvent) {
		if !settings.GetBool(settings.TelemetryKey) {
			return
		}
		if evt.Old != nil && reflect.DeepEqual(evt.Old.OTEL, evt.New.OTEL) {
			// no changes to telemetry config, no need to update
			return
		}
		if err := telemetry.Initialize(r.deviceID, *evt.New, settings.IsPro()); err != nil {
			slog.Error("Failed to update telemetry config", "error", err)
		}
	})
	r.telemetryCfgSub.Store(sub)

	// subscribe to VPN status events to start/stop connection metrics collection
	r.vpnStatusSub = events.Subscribe(func(evt vpn.StatusUpdateEvent) {
		r.updateConnMetrics(evt.Status)
	})
	return nil
}

func (r *LocalBackend) stopTelemetry() {
	if sub := r.telemetryCfgSub.Swap(nil); sub != nil {
		sub.Unsubscribe()
	}
	if r.vpnStatusSub != nil {
		r.vpnStatusSub.Unsubscribe()
		r.vpnStatusSub = nil
	}
	r.stopConnMetricsIfRunning()
	telemetry.Close()
}

// updateConnMetrics starts or stops connection metrics collection based on VPN status.
// Metrics are only collected when the VPN is connected and telemetry is enabled.
func (r *LocalBackend) updateConnMetrics(status vpn.VPNStatus) {
	if status == vpn.Connected {
		r.startConnMetrics()
	} else {
		r.stopConnMetricsIfRunning()
	}
}

func (r *LocalBackend) startConnMetrics() {
	r.connMetricsMu.Lock()
	defer r.connMetricsMu.Unlock()
	if r.stopConnMetrics != nil {
		return // already running
	}
	r.stopConnMetrics = telemetry.StartConnectionMetrics(r.ctx, r.vpnClient, 1*time.Minute)
	slog.Debug("Started connection metrics collection")
}

func (r *LocalBackend) stopConnMetricsIfRunning() {
	r.connMetricsMu.Lock()
	defer r.connMetricsMu.Unlock()
	if r.stopConnMetrics != nil {
		r.stopConnMetrics()
		r.stopConnMetrics = nil
		slog.Debug("Stopped connection metrics collection")
	}
}

///////////////////////
// Server management //
///////////////////////

func (r *LocalBackend) Servers() servers.Servers {
	return r.srvManager.Servers()
}

func (r *LocalBackend) GetServerByTag(tag string) (servers.Server, bool) {
	return r.srvManager.GetServerByTag(tag)
}

func (r *LocalBackend) AddServers(group servers.ServerGroup, options servers.Options) error {
	if err := r.srvManager.AddServers(group, options, true); err != nil {
		return fmt.Errorf("failed to add servers to ServerManager: %w", err)
	}
	if err := r.vpnClient.AddOutbounds(group, options); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		return fmt.Errorf("failed to add outbounds to VPN client: %w", err)
	}
	return nil
}

func (r *LocalBackend) RemoveServers(tags []string) error {
	removed, err := r.srvManager.RemoveServers(tags)
	if err != nil {
		return fmt.Errorf("failed to remove servers from ServerManager: %w", err)
	}
	servers := make(map[string][]string)
	for _, srv := range removed {
		servers[srv.Group] = append(servers[srv.Group], srv.Tag)
	}
	for group, tags := range servers {
		if err := r.vpnClient.RemoveOutbounds(group, tags); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
			return fmt.Errorf("failed to remove outbounds from VPN client: %w", err)
		}
	}
	return nil
}

func (r *LocalBackend) setServers(group servers.ServerGroup, options servers.Options) error {
	if err := r.srvManager.SetServers(group, options); err != nil {
		return fmt.Errorf("failed to set servers in ServerManager: %w", err)
	}
	err := r.vpnClient.UpdateOutbounds(group, options)
	if err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		slog.Error("Failed to update VPN outbounds after config change", "error", err)
	}
	return nil
}

func (r *LocalBackend) AddServersByJSON(config string) error {
	return r.srvManager.AddServersByJSON(context.Background(), []byte(config))
}

func (r *LocalBackend) AddServersByURL(urls []string, skipCertVerification bool) error {
	return r.srvManager.AddServersByURL(context.Background(), urls, skipCertVerification)
}

func (r *LocalBackend) AddPrivateServer(tag, ip string, port int, accessToken string, loc C.ServerLocation, joined bool) error {
	return r.srvManager.AddPrivateServer(tag, ip, port, accessToken, loc, joined)
}

func (r *LocalBackend) InviteToPrivateServer(ip string, port int, accessToken string, inviteName string) (string, error) {
	return r.srvManager.InviteToPrivateServer(ip, port, accessToken, inviteName)
}

func (r *LocalBackend) RevokePrivateServerInvite(ip string, port int, accessToken string, inviteName string) error {
	return r.srvManager.RevokePrivateServerInvite(ip, port, accessToken, inviteName)
}

/////////////////
//     VPN     //
/////////////////

func (r *LocalBackend) VPNStatus() vpn.VPNStatus {
	return r.vpnClient.Status()
}

func (r *LocalBackend) ConnectVPN(tag string) error {
	if tag != vpn.AutoSelectTag {
		if _, found := r.srvManager.GetServerByTag(tag); !found {
			return fmt.Errorf("no server found with tag %s", tag)
		}
	}
	bOptions := r.getBoxOptions()
	if err := r.vpnClient.Connect(bOptions); err != nil {
		return fmt.Errorf("failed to connect VPN: %w", err)
	}
	if err := r.selectServer(tag); err != nil {
		return fmt.Errorf("failed to select server: %w", err)
	}
	return nil
}

func (r *LocalBackend) getBoxOptions() vpn.BoxOptions {
	// ignore error, we can still connect with default options if config is not available for some reason
	cfg, _ := r.confHandler.GetConfig()
	bOptions := vpn.BoxOptions{
		BasePath: settings.GetString(settings.DataPathKey),
	}
	if cfg != nil {
		bOptions.Options = cfg.Options
		bOptions.BanditURLOverrides = cfg.BanditURLOverrides
		bOptions.BanditThroughputURL = cfg.BanditThroughputURL
		if settings.GetBool(settings.SmartRoutingKey) {
			bOptions.SmartRouting = cfg.SmartRouting
		}
		if settings.GetBool(settings.AdBlockKey) {
			bOptions.AdBlock = cfg.AdBlock
		}
	}
	if userServers, ok := r.srvManager.Servers()[servers.SGUser]; ok {
		bOptions.UserServers = option.Options{
			Outbounds: userServers.Outbounds,
			Endpoints: userServers.Endpoints,
		}
	}
	return bOptions
}

func (r *LocalBackend) DisconnectVPN() error {
	return r.vpnClient.Disconnect()
}

func (r *LocalBackend) RestartVPN() error {
	bOptions := r.getBoxOptions()
	return r.vpnClient.Restart(bOptions)
}

func (r *LocalBackend) SelectServer(tag string) error {
	return r.selectServer(tag)
}

func (r *LocalBackend) selectServer(tag string) error {
	var server servers.Server
	switch tag {
	case vpn.AutoSelectTag:
		server = servers.Server{Group: vpn.AutoSelectTag, Tag: vpn.AutoSelectTag}
	case vpn.AutoLanternTag:
		server = servers.Server{Group: servers.SGLantern, Tag: vpn.AutoLanternTag}
	case vpn.AutoUserTag:
		server = servers.Server{Group: servers.SGUser, Tag: vpn.AutoUserTag}
	default:
		var found bool
		if server, found = r.srvManager.GetServerByTag(tag); !found {
			return fmt.Errorf("no server found with tag %s", tag)
		}
	}
	if err := r.vpnClient.SelectServer(server.Group, tag); err != nil {
		return fmt.Errorf("failed to select server: %w", err)
	}

	server.Options = nil
	if err := settings.Set(settings.SelectedServerKey, server); err != nil {
		slog.Warn("Failed to save selected server in settings", "error", err)
	}
	slog.Info("Selected server", "tag", tag, "group", server.Group, "type", server.Type)
	return nil
}

// Connections returns a list of all connections, both active and recently closed. If there are no
// connections and the tunnel is open, an empty slice is returned without an error.
func (r *LocalBackend) VPNConnections() ([]vpn.Connection, error) {
	return r.vpnClient.Connections()
}

// ActiveConnections returns a list of currently active connections, ordered from newest to oldest.
func (r *LocalBackend) ActiveVPNConnections() ([]vpn.Connection, error) {
	connections, err := r.vpnClient.Connections()
	if err != nil {
		return nil, fmt.Errorf("failed to get VPN connections: %w", err)
	}
	connections = slices.DeleteFunc(connections, func(conn vpn.Connection) bool {
		return conn.ClosedAt != 0
	})
	slices.SortFunc(connections, func(a, b vpn.Connection) int {
		return int(b.CreatedAt - a.CreatedAt)
	})
	return connections, nil
}

// SelectedServer returns the currently selected server and whether the server is still available.
// The server may no longer be available if it was removed from the manager since it was selected.
func (r *LocalBackend) SelectedServer() (servers.Server, bool, error) {
	var selected servers.Server
	if settings.Exists(settings.SelectedServerKey) {
		settings.GetStruct(settings.SelectedServerKey, &selected)
	}
	if selected == (servers.Server{}) {
		// the selected server hasn't been stored yet, or it wasn't stored as a Server, so fall back
		// to asking the VPN client for the selected server
		_, tag, err := r.vpnClient.GetSelected()
		if err != nil {
			return servers.Server{}, false, fmt.Errorf("failed to get selected server from VPN client: %w", err)
		}
		server, found := r.srvManager.GetServerByTag(tag)
		if !found {
			// this should never happen since the options are only generated from servers in the manager,
			// but log just in case
			slog.Warn("Selected server from VPN client not found in ServerManager", "tag", tag)
		}
		return server, found, nil
	}
	server, found := r.srvManager.GetServerByTag(selected.Tag)
	stillExists := found &&
		server.Group == selected.Group &&
		server.Type == selected.Type &&
		server.Location == selected.Location
	return selected, stillExists, nil
}

func (r *LocalBackend) ActiveServer() (servers.Server, error) {
	group, tag, err := r.vpnClient.ActiveServer()
	if err != nil {
		return servers.Server{}, fmt.Errorf("failed to get active server from VPN client: %w", err)
	}
	server, found := r.srvManager.GetServerByTag(tag)
	if !found {
		return servers.Server{
			Group: group,
			Tag:   tag,
		}, fmt.Errorf("active server from VPN client not found in ServerManager: %s", tag)
	}
	return server, nil
}

func (r *LocalBackend) RunOfflineURLTests() error {
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		return fmt.Errorf("no config available: %w", err)
	}
	return r.vpnClient.RunOfflineURLTests(
		settings.GetString(settings.DataPathKey),
		cfg.Options.Outbounds,
	)
}

// AutoServerSelections returns the currently active server for each auto server group.
func (r *LocalBackend) AutoServerSelections() (vpn.AutoSelections, error) {
	return r.vpnClient.AutoServerSelections()
}

// StartAutoSelectionsListener starts polling for auto-selection changes and emitting events.
func (r *LocalBackend) StartAutoSelectionsListener() {
	r.vpnClient.AutoSelectionsChangeListener(r.ctx)
}

//////////////////
// Split Tunnel //
/////////////////

func (r *LocalBackend) SplitTunnelFilters() vpn.SplitTunnelFilter {
	return r.splitTunnelMgr.Filters()
}

func (r *LocalBackend) AddSplitTunnelItems(items vpn.SplitTunnelFilter) error {
	return r.splitTunnelMgr.AddItems(items)
}

func (r *LocalBackend) RemoveSplitTunnelItems(items vpn.SplitTunnelFilter) error {
	return r.splitTunnelMgr.RemoveItems(items)
}

/////////////
// Account //
/////////////

func (r *LocalBackend) NewUser(ctx context.Context) (*account.UserData, error) {
	return r.accountClient.NewUser(ctx)
}

func (r *LocalBackend) Login(ctx context.Context, email, password string) (*account.UserData, error) {
	return r.accountClient.Login(ctx, email, password)
}

func (r *LocalBackend) Logout(ctx context.Context, email string) (*account.UserData, error) {
	return r.accountClient.Logout(ctx, email)
}

func (r *LocalBackend) FetchUserData(ctx context.Context) (*account.UserData, error) {
	return r.accountClient.FetchUserData(ctx)
}

func (r *LocalBackend) StartChangeEmail(ctx context.Context, newEmail, password string) error {
	return r.accountClient.StartChangeEmail(ctx, newEmail, password)
}

func (r *LocalBackend) CompleteChangeEmail(ctx context.Context, newEmail, password, code string) error {
	return r.accountClient.CompleteChangeEmail(ctx, newEmail, password, code)
}

func (r *LocalBackend) StartRecoveryByEmail(ctx context.Context, email string) error {
	return r.accountClient.StartRecoveryByEmail(ctx, email)
}

func (r *LocalBackend) CompleteRecoveryByEmail(ctx context.Context, email, newPassword, code string) error {
	return r.accountClient.CompleteRecoveryByEmail(ctx, email, newPassword, code)
}

func (r *LocalBackend) DeleteAccount(ctx context.Context, email, password string) (*account.UserData, error) {
	return r.accountClient.DeleteAccount(ctx, email, password)
}

func (r *LocalBackend) SignUp(ctx context.Context, email, password string) ([]byte, *account.SignupResponse, error) {
	return r.accountClient.SignUp(ctx, email, password)
}

func (r *LocalBackend) SignupEmailConfirmation(ctx context.Context, email, code string) error {
	return r.accountClient.SignupEmailConfirmation(ctx, email, code)
}

func (r *LocalBackend) SignupEmailResendCode(ctx context.Context, email string) error {
	return r.accountClient.SignupEmailResendCode(ctx, email)
}

func (r *LocalBackend) ValidateEmailRecoveryCode(ctx context.Context, email, code string) error {
	return r.accountClient.ValidateEmailRecoveryCode(ctx, email, code)
}

func (r *LocalBackend) DataCapInfo(ctx context.Context) (string, error) {
	return r.accountClient.DataCapInfo(ctx)
}

func (r *LocalBackend) DataCapStream(ctx context.Context) error {
	return r.accountClient.DataCapStream(ctx)
}

func (r *LocalBackend) RemoveDevice(ctx context.Context, deviceID string) (*account.LinkResponse, error) {
	return r.accountClient.RemoveDevice(ctx, deviceID)
}

func (r *LocalBackend) OAuthLoginCallback(ctx context.Context, oAuthToken string) (*account.UserData, error) {
	return r.accountClient.OAuthLoginCallback(ctx, oAuthToken)
}

func (r *LocalBackend) OAuthLoginUrl(ctx context.Context, provider string) (string, error) {
	return r.accountClient.OAuthLoginURL(ctx, provider)
}

func (r *LocalBackend) UserDevices() ([]settings.Device, error) {
	return settings.Devices()
}

func (r *LocalBackend) UserData() (*account.UserData, error) {
	var userData account.UserData
	if err := settings.GetStruct(settings.UserDataKey, &userData); err != nil {
		return nil, fmt.Errorf("failed to get user data from settings: %w", err)
	}
	return &userData, nil
}

///////////////////
// Subscriptions //
///////////////////

func (r *LocalBackend) ActivationCode(ctx context.Context, email, resellerCode string) (*account.PurchaseResponse, error) {
	return r.accountClient.ActivationCode(ctx, email, resellerCode)
}

func (r *LocalBackend) NewStripeSubscription(ctx context.Context, email, planID string) (string, error) {
	return r.accountClient.NewStripeSubscription(ctx, email, planID)
}

func (r *LocalBackend) PaymentRedirect(ctx context.Context, data account.PaymentRedirectData) (string, error) {
	return r.accountClient.PaymentRedirect(ctx, data)
}

func (r *LocalBackend) ReferralAttach(ctx context.Context, code string) (bool, error) {
	return r.accountClient.ReferralAttach(ctx, code)
}

func (r *LocalBackend) StripeBillingPortalURL(ctx context.Context, baseURL, userID, proToken string) (string, error) {
	return r.accountClient.StripeBillingPortalURL(ctx, baseURL, userID, proToken)
}

func (r *LocalBackend) SubscriptionPaymentRedirectURL(ctx context.Context, data account.PaymentRedirectData) (string, error) {
	return r.accountClient.SubscriptionPaymentRedirectURL(ctx, data)
}

func (r *LocalBackend) SubscriptionPlans(ctx context.Context, channel string) (string, error) {
	return r.accountClient.SubscriptionPlans(ctx, channel)
}

func (r *LocalBackend) VerifySubscription(ctx context.Context, service account.SubscriptionService, data map[string]string) (string, error) {
	return r.accountClient.VerifySubscription(ctx, service, data)
}
