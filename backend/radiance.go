// Package backend provides the main interface for all the major components of Radiance.
package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"time"

	"github.com/Xuanwo/go-locale"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	C "github.com/getlantern/common"
	"github.com/getlantern/publicip"

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

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
)

const tracerName = "github.com/getlantern/radiance/backend"

// LocalBackend ties all the core functionality of Radiance together. It manages the configuration,
// servers, VPN connection, account management, issue reporting, and telemetry for the application.
type LocalBackend struct {
	ctx    context.Context
	cancel context.CancelFunc

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

	telemetryCfgSub *events.Subscription[config.NewConfigEvent]
	stopConnMetrics context.CancelFunc
	connMetricsMu   sync.Mutex

	dataCapCh   chan *account.DataCapInfo // latest datacap update; nil when stream not running
	stopDataCap context.CancelFunc
	dataCapMu   sync.Mutex

	stopURLTestListener context.CancelFunc
	urlTestMu           sync.Mutex
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
	// EnvOverrides are applied via os.Setenv before common.Init so sandboxed
	// system extensions (macOS/iOS), which don't inherit shell env, still see
	// RADIANCE_* vars from the host process. Entries are set verbatim — no
	// filtering.
	EnvOverrides map[string]string
}

// NewLocalBackend performs global initialization and returns a new LocalBackend instance.
// It should be called once at the start of the application.
func NewLocalBackend(ctx context.Context, opts Options) (*LocalBackend, error) {
	// Must run before common.Init: it reads RADIANCE_VERSION once and
	// freezes it, so a later Setenv is ignored by the header-fill path.
	var envOverrideErrs error
	for k, v := range opts.EnvOverrides {
		if err := os.Setenv(k, v); err != nil {
			envOverrideErrs = errors.Join(envOverrideErrs, fmt.Errorf("apply env override %q: %w", k, err))
		}
	}
	if envOverrideErrs != nil {
		return nil, fmt.Errorf("failed to apply environment overrides: %w", envOverrideErrs)
	}
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
		platformDeviceID = deviceid.Get(settings.GetString(settings.DataPathKey))
	}

	dataDir := settings.GetString(settings.DataPathKey)
	disableFetch := env.GetBool(env.DisableFetch)
	settings.Patch(settings.Settings{
		settings.LocaleKey:              opts.Locale,
		settings.DeviceIDKey:            platformDeviceID,
		settings.ConfigFetchDisabledKey: disableFetch,
		settings.TelemetryKey:           opts.TelemetryConsent,
	})

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

	vpnClient := vpn.NewVPNClient(dataDir, slog.Default().With("service", "vpn"), opts.PlatformInterface)
	ctx, cancel := context.WithCancel(ctx)
	cOpts := config.Options{
		DataPath:      dataDir,
		Locale:        opts.Locale,
		AccountClient: accountClient,
		HTTPClient:    kindling.HTTPClient(),
		Logger:        slog.Default().With("service", "config_handler"),
	}
	r := &LocalBackend{
		ctx:            ctx,
		cancel:         cancel,
		issueReporter:  issue.NewIssueReporter(kindling.HTTPClient()),
		accountClient:  accountClient,
		confHandler:    config.NewConfigHandler(ctx, cOpts),
		srvManager:     svrMgr,
		vpnClient:      vpnClient,
		splitTunnelMgr: splitTunnelMgr,
		shutdownFuncs: []func() error{
			telemetry.Close, kindling.Close,
		},
		stopChan:  make(chan struct{}),
		closeOnce: sync.Once{},
		deviceID:  platformDeviceID,
		dataCapCh: make(chan *account.DataCapInfo, 1),
	}
	return r, nil
}

func (r *LocalBackend) Start() {
	// eagerly start kindling so it's ready by the time we need to make network requests
	kindling.Init()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		result, err := publicip.Detect(ctx, &publicip.Config{
			Timeout:      2 * time.Second,
			MinConsensus: 1,
		})
		cancel()
		if err != nil {
			slog.Warn("Failed to get public IP", "error", err)
		} else {
			common.SetPublicIP(result.IP.String())
			slog.Debug("Detected public IP", "confidence", result.Confidence, "sources", result.Sources)
		}
	}()

	if settings.GetBool(settings.TelemetryKey) {
		if err := r.startTelemetry(); err != nil {
			slog.Error("Failed to start telemetry", "error", err)
		}
	}
	r.startVPNStatusListeners()
	r.startAutoSelectedListener()

	// set country code in settings when new config is received so it can be included in issue reports
	events.SubscribeOnce(func(evt config.NewConfigEvent) {
		if env.GetString(env.Country) != "" {
			return // respect env override if set
		}
		if evt.New != nil && evt.New.Country != "" {
			if err := settings.Set(settings.CountryCodeKey, evt.New.Country); err != nil {
				slog.Error("failed to set country code in settings", "error", err)
			}
			slog.Info("Set country code from config response", "country_code", evt.New.Country)
		}
	})
	// update VPN outbounds when new config is received
	events.SubscribeContext(r.ctx, func(evt config.NewConfigEvent) {
		if evt.New == nil {
			return
		}
		cfg := evt.New
		locs := make(map[string]C.ServerLocation, len(cfg.OutboundLocations))
		// Track which cities are already covered by active outbounds.
		coveredCities := make(map[string]bool, len(cfg.OutboundLocations))
		for k, v := range cfg.OutboundLocations {
			if v == nil {
				slog.Warn("Server location is nil, skipping", "tag", k)
				continue
			}
			locs[k] = *v
			coveredCities[v.City+"|"+v.CountryCode] = true
		}
		// Include available server locations not already covered by active
		// outbounds so the client's location picker shows every location.
		for _, sl := range cfg.Servers {
			if coveredCities[sl.City+"|"+sl.CountryCode] {
				continue
			}
			key := strings.ToLower(strings.ReplaceAll(sl.City, " ", "-") + "-" + sl.CountryCode)
			locs[key] = sl
		}
		var srvs []*servers.Server
		for _, out := range cfg.Options.Outbounds {
			srvs = append(srvs, &servers.Server{
				Tag: out.Tag, Type: out.Type, IsLantern: true,
				Options: out, Location: locs[out.Tag],
			})
		}
		for _, ep := range cfg.Options.Endpoints {
			srvs = append(srvs, &servers.Server{
				Tag: ep.Tag, Type: ep.Type, IsLantern: true,
				Options: ep, Location: locs[ep.Tag],
			})
		}
		list := servers.ServerList{Servers: srvs, URLOverrides: cfg.BanditURLOverrides}
		if len(cfg.BanditURLOverrides) > 0 {
			// Create a marker span linked to the API's bandit trace so the
			// config fetch appears in the same distributed trace as the callback.
			if ctx, ok := traces.ExtractBanditTraceContext(cfg.BanditURLOverrides); ok {
				_, span := otel.Tracer(tracerName).Start(ctx, "radiance.config_received",
					trace.WithAttributes(
						attribute.Int("bandit.override_count", len(cfg.BanditURLOverrides)),
						attribute.Int("bandit.outbound_count", len(cfg.Options.Outbounds)),
					),
				)
				span.End() // point-in-time marker — config was received at this timestamp
			}
		}
		if err := r.setServers(list, true); err != nil {
			slog.Error("setting servers in manager", "error", err)
		}
		if err := r.RunOfflineURLTests(); err != nil {
			slog.Error("Failed to run offline URL tests after config update", "error", err)
		}
	})
	go r.confHandler.Start()
}

func (r *LocalBackend) Close() {
	r.closeOnce.Do(func() {
		slog.Debug("Closing Radiance")
		r.cancel() // cancels context, unsubscribes all event listeners and stops child goroutines
		close(r.stopChan)
		for _, shutdown := range r.shutdownFuncs {
			if err := shutdown(); err != nil {
				slog.Error("Failed to shutdown", "error", err)
			}
		}
	})
	<-r.stopChan
}

func (r *LocalBackend) startVPNStatusListeners() {
	events.SubscribeContext(r.ctx, func(evt vpn.StatusUpdateEvent) {
		r.updateConnMetrics(evt.Status)
	})
	events.SubscribeContext(r.ctx, func(evt vpn.StatusUpdateEvent) {
		r.updateDataCapStream(evt.Status)
	})
	events.SubscribeContext(r.ctx, func(evt vpn.StatusUpdateEvent) {
		r.updateURLTestListener(evt.Status)
	})
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

	attachments := baseIssueAttachments()
	if r.splitTunnelMgr.IsEnabled() {
		attachments = append(attachments, filepath.Join(settings.GetString(settings.DataPathKey), internal.SplitTunnelFileName))
	}
	attachments = append(attachments, additionalAttachments...)

	report := issue.IssueReport{
		Type:                  issueType,
		Description:           description,
		Email:                 email,
		CountryCode:           country,
		DeviceID:              r.deviceID,
		UserID:                settings.GetString(settings.UserIDKey),
		SubscriptionLevel:     settings.GetString(settings.UserLevelKey),
		Locale:                settings.GetString(settings.LocaleKey),
		AdditionalAttachments: attachments,
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
	// TODO: any other files we want to include??
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

// UpdateConfig forces an immediate fetch of the latest configuration. It returns
// [config.ErrConfigFetchDisabled] if config fetching is disabled in settings.
func (r *LocalBackend) UpdateConfig() error {
	return r.confHandler.Update()
}

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
		slog.Info("Restarting VPN to apply new settings", "ad_block_changed", adBlockChanged, "smart_routing_changed", smartRoutingChanged)
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
	if r.telemetryCfgSub != nil {
		return nil
	}
	// subscribe to config changes to update telemetry config
	r.telemetryCfgSub = events.SubscribeContext(r.ctx, func(evt config.NewConfigEvent) {
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
	return nil
}

func (r *LocalBackend) stopTelemetry() {
	if r.telemetryCfgSub != nil {
		r.telemetryCfgSub.Unsubscribe()
		r.telemetryCfgSub = nil
	}
	r.updateConnMetrics(vpn.Disconnected)
	telemetry.Close()
}

func (r *LocalBackend) updateConnMetrics(status vpn.VPNStatus) {
	if !settings.GetBool(settings.TelemetryKey) {
		return
	}
	r.connMetricsMu.Lock()
	defer r.connMetricsMu.Unlock()
	if status == vpn.Connected {
		if r.stopConnMetrics != nil {
			return // already running
		}
		ctx, cancel := context.WithCancel(r.ctx)
		telemetry.StartConnectionMetrics(ctx, r.vpnClient, 1*time.Minute)
		r.stopConnMetrics = cancel
		slog.Debug("Started connection metrics collection")
	} else if r.stopConnMetrics != nil {
		r.stopConnMetrics()
		r.stopConnMetrics = nil
		slog.Debug("Stopped connection metrics collection")
	}
}

///////////////////////
// Server management //
///////////////////////

func (r *LocalBackend) AllServers() []*servers.Server {
	return r.srvManager.AllServers()
}

func (r *LocalBackend) GetServerByTag(tag string) (*servers.Server, bool) {
	return r.srvManager.GetServerByTag(tag)
}

func (r *LocalBackend) AddServers(list servers.ServerList) error {
	if err := r.srvManager.AddServers(list, false); err != nil {
		return fmt.Errorf("failed to add servers to ServerManager: %w", err)
	}
	if err := r.vpnClient.AddOutbounds(list); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		return fmt.Errorf("failed to add outbounds to VPN client: %w", err)
	}
	return nil
}

func (r *LocalBackend) RemoveServers(tags []string) error {
	removed, err := r.srvManager.RemoveServers(tags)
	if err != nil {
		return fmt.Errorf("failed to remove servers from ServerManager: %w", err)
	}
	removedTags := make([]string, 0, len(removed))
	for _, srv := range removed {
		removedTags = append(removedTags, srv.Tag)
	}
	if len(removedTags) > 0 {
		var selected servers.Server
		if err := settings.GetStruct(settings.SelectedServerKey, &selected); err == nil {
			if slices.Contains(removedTags, selected.Tag) {
				// clear selected server from settings if it's being removed
				if err := settings.Set(settings.SelectedServerKey, nil); err != nil {
					slog.Warn("Failed to clear selected server from settings after it was removed", "error", err)
				}
			}
		}
		if err := r.vpnClient.RemoveOutbounds(removedTags); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
			return fmt.Errorf("failed to remove outbounds: %w", err)
		}
	}
	return nil
}

func (r *LocalBackend) setServers(list servers.ServerList, isLantern bool) error {
	if err := r.srvManager.SetServers(list, isLantern); err != nil {
		return fmt.Errorf("failed to set servers in ServerManager: %w", err)
	}
	err := r.vpnClient.UpdateOutbounds(list)
	if err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		slog.Error("Failed to update VPN outbounds after config change", "error", err)
	}
	return nil
}

func (r *LocalBackend) AddServersByJSON(config string) ([]string, error) {
	return r.srvManager.AddServersByJSON(context.Background(), []byte(config))
}

func (r *LocalBackend) AddServersByURL(urls []string, skipCertVerification bool) ([]string, error) {
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

// urlTestFlushInterval bounds how often URL test results are written back to the servers manager
// (and to disk). URL test cycles run on the order of minutes and notify per-result, so coalescing
// into a periodic flush avoids re-marshalling and re-writing the servers file for each parallel result.
const urlTestFlushInterval = 5 * time.Second

func (r *LocalBackend) updateURLTestListener(status vpn.VPNStatus) {
	r.urlTestMu.Lock()
	defer r.urlTestMu.Unlock()
	if status == vpn.Connected {
		if r.stopURLTestListener != nil {
			return // already running
		}
		storage := r.vpnClient.HistoryStorage()
		if storage == nil {
			return
		}
		ctx, cancel := context.WithCancel(r.ctx)
		r.stopURLTestListener = cancel
		hook := make(chan struct{}, 1)
		storage.SetHook(hook)
		go r.runURLTestListener(ctx, storage, hook)
		slog.Debug("Started URL test result listener")
	} else if r.stopURLTestListener != nil {
		r.stopURLTestListener()
		r.stopURLTestListener = nil
		slog.Debug("Stopped URL test result listener")
	}
}

// runURLTestListener coalesces per-result hook notifications into a periodic flush so the servers
// file isn't rewritten for each parallel URL test completion. A final flush runs on shutdown so any
// results that arrived since the last tick are persisted.
func (r *LocalBackend) runURLTestListener(ctx context.Context, storage vpn.URLTestHistoryStorage, hook <-chan struct{}) {
	ticker := time.NewTicker(urlTestFlushInterval)
	defer ticker.Stop()
	dirty := true // start dirty so we persist any results that arrived before the listener started
	for {
		select {
		case <-ctx.Done():
			if dirty {
				r.flushURLTestResults(storage)
			}
			return
		case <-hook:
			dirty = true
		case <-ticker.C:
			if dirty {
				r.flushURLTestResults(storage)
				dirty = false
			}
		}
	}
}

func (r *LocalBackend) flushURLTestResults(storage vpn.URLTestHistoryStorage) {
	results := make(map[string]servers.URLTestResult)
	for _, srv := range r.srvManager.AllServers() {
		if h := storage.LoadURLTestHistory(srv.Tag); h != nil {
			results[srv.Tag] = servers.URLTestResult{Delay: h.Delay, Time: h.Time}
		}
	}
	if len(results) > 0 {
		if err := r.srvManager.UpdateURLTestResults(results); err != nil {
			slog.Warn("Failed to persist URL test results", "error", err)
		}
	}
}

/////////////////
//     VPN     //
/////////////////

func (r *LocalBackend) VPNStatus() vpn.VPNStatus {
	return r.vpnClient.Status()
}

func (r *LocalBackend) ConnectVPN(tag string) error {
	if tag == "" {
		tag = vpn.AutoSelectTag
	}
	if tag != vpn.AutoSelectTag {
		if _, found := r.srvManager.GetServerByTag(tag); !found {
			return fmt.Errorf("no server found with tag %s", tag)
		}
	}
	bOptions := r.getBoxOptions()
	if err := r.vpnClient.Connect(bOptions); err != nil {
		return fmt.Errorf("failed to connect VPN: %w", err)
	}
	if err := r.SelectServer(tag); err != nil {
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
	seed := make(map[string]adapter.URLTestHistory)
	for _, srv := range r.srvManager.AllServers() {
		if !srv.IsLantern {
			switch opts := srv.Options.(type) {
			case option.Outbound:
				bOptions.Options.Outbounds = append(bOptions.Options.Outbounds, opts)
			case option.Endpoint:
				bOptions.Options.Endpoints = append(bOptions.Options.Endpoints, opts)
			}
		}
		if srv.URLTestResult != nil {
			seed[srv.Tag] = adapter.URLTestHistory{
				Time:  srv.URLTestResult.Time,
				Delay: srv.URLTestResult.Delay,
			}
		}
	}
	if len(seed) > 0 {
		bOptions.URLTestSeed = seed
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

// SelectServer selects the server identified by tag. The empty string is
// treated as [vpn.AutoSelectTag].
func (r *LocalBackend) SelectServer(tag string) error {
	if tag == "" {
		tag = vpn.AutoSelectTag
	}
	if err := r.vpnClient.SelectServer(tag); err != nil {
		return fmt.Errorf("failed to select server: %w", err)
	}
	if tag == vpn.AutoSelectTag {
		err := settings.Patch(settings.Settings{
			settings.AutoConnectKey:    true,
			settings.SelectedServerKey: nil,
		})
		if err != nil {
			slog.Warn("failed to update settings", "error", err)
		}
		return nil
	}

	server, found := r.srvManager.GetServerByTag(tag)
	if !found { // sanity check, the vpn should have errored if this were the case
		return fmt.Errorf("no server found with tag %s", tag)
	}
	server.Options = nil
	err := settings.Patch(settings.Settings{
		settings.AutoConnectKey:    false,
		settings.SelectedServerKey: server,
	})
	if err != nil {
		slog.Warn("Failed to save selected server in settings", "error", err)
	}
	slog.Info("Selected server", "tag", tag, "type", server.Type)
	return nil
}

// VPNConnections returns a list of all connections, both active and recently closed. If there are no
// connections and the tunnel is open, an empty slice is returned without an error.
func (r *LocalBackend) VPNConnections() ([]vpn.Connection, error) {
	return r.vpnClient.Connections()
}

// ActiveVPNConnections returns a list of currently active connections, ordered from newest to oldest.
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

// TODO: handle case where selected server is no longer available (e.g. removed from manager) more
// gracefully, currently we just return that the server is no longer available but maybe we should
// also clear the selected server from settings and select a new server in the VPN client.
// should we not remove a lantern server if it's currently selected in the VPN client and instead
// mark it as unavailable in the manager until it's no longer selected in the VPN client?

// SelectedServer returns the currently selected server and whether the server is still available.
// The server may no longer be available if it was removed from the manager since it was selected.
func (r *LocalBackend) SelectedServer() (*servers.Server, bool, error) {
	if settings.GetBool(settings.AutoConnectKey) {
		tag, err := r.vpnClient.CurrentAutoSelectedServer()
		if err != nil {
			return nil, false, fmt.Errorf("failed to get current auto-selected server: %w", err)
		}
		server, found := r.srvManager.GetServerByTag(tag)
		return server, found, nil
	}
	if !settings.Exists(settings.SelectedServerKey) {
		return nil, false, fmt.Errorf("no selected server")
	}
	var selected servers.Server
	if err := settings.GetStruct(settings.SelectedServerKey, &selected); err != nil {
		return nil, false, fmt.Errorf("failed to get selected server from settings: %w", err)
	}
	server, found := r.srvManager.GetServerByTag(selected.Tag)
	stillExists := found &&
		server.IsLantern == selected.IsLantern &&
		server.Type == selected.Type &&
		server.Location == selected.Location
	return &selected, stillExists, nil
}

// CurrentAutoSelectedServer returns the tag of the server that is currently auto-selected by the
// VPN client.
func (r *LocalBackend) CurrentAutoSelectedServer() (string, error) {
	return r.vpnClient.CurrentAutoSelectedServer()
}

func (r *LocalBackend) startAutoSelectedListener() {
	var (
		mu     sync.Mutex
		cancel context.CancelFunc
	)
	events.SubscribeContext(r.ctx, func(evt vpn.StatusUpdateEvent) {
		mu.Lock()
		defer mu.Unlock()
		if cancel != nil {
			cancel()
			cancel = nil
		}
		if evt.Status == vpn.Connected {
			var ctx context.Context
			ctx, cancel = context.WithCancel(r.ctx)
			r.vpnClient.AutoSelectedChangeListener(ctx)
		}
	})
}

func (r *LocalBackend) RunOfflineURLTests() error {
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		return fmt.Errorf("no config available: %w", err)
	}
	svrs := r.srvManager.AllServers()
	slog.Debug("Running offline URL tests", "server_count", len(svrs), "url_override_count", len(cfg.BanditURLOverrides))
	results, err := r.vpnClient.RunOfflineURLTests(
		settings.GetString(settings.DataPathKey),
		servers.ServerList{Servers: svrs}.Outbounds(),
		cfg.BanditURLOverrides,
	)
	if err != nil {
		return err
	}
	now := time.Now()
	urlResults := make(map[string]servers.URLTestResult, len(results))
	for tag, delay := range results {
		urlResults[tag] = servers.URLTestResult{Delay: delay, Time: now}
	}
	if len(urlResults) > 0 {
		if err := r.srvManager.UpdateURLTestResults(urlResults); err != nil {
			slog.Warn("Failed to persist offline URL test results", "error", err)
		}
		selected, err := r.vpnClient.CurrentAutoSelectedServer()
		if err != nil {
			slog.Warn("Failed to get current auto-selected server after URL tests", "error", err)
		} else {
			events.Emit(vpn.AutoSelectedEvent{Selected: selected})
		}
	}
	return nil
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

func (r *LocalBackend) DataCapInfo(ctx context.Context) (*account.DataCapInfo, error) {
	return r.accountClient.DataCapInfo(ctx)
}

// DataCapUpdates returns the channel that receives datacap updates from the
// upstream SSE stream. The stream runs while the VPN is connected; the channel
// is never closed so callers should select on it alongside a context or other
// signal.
func (r *LocalBackend) DataCapUpdates() <-chan *account.DataCapInfo {
	return r.dataCapCh
}

func (r *LocalBackend) updateDataCapStream(status vpn.VPNStatus) {
	r.dataCapMu.Lock()
	defer r.dataCapMu.Unlock()
	if status == vpn.Connected {
		if r.stopDataCap != nil {
			return // already running
		}
		ctx, cancel := context.WithCancel(r.ctx)
		r.stopDataCap = cancel
		go func() {
			_ = r.accountClient.DataCapStream(ctx, func(info *account.DataCapInfo) {
				// Non-blocking send; drops stale updates if the reader is slow.
				select {
				case r.dataCapCh <- info:
				default:
					select {
					case <-r.dataCapCh:
					default:
					}
					r.dataCapCh <- info
				}
			})
		}()
		slog.Debug("Started datacap SSE stream")
	} else if r.stopDataCap != nil {
		r.stopDataCap()
		r.stopDataCap = nil
		slog.Debug("Stopped datacap SSE stream")
	}
}

func (r *LocalBackend) RemoveDevice(ctx context.Context, deviceID string) (*account.LinkResponse, error) {
	return r.accountClient.RemoveDevice(ctx, deviceID)
}

func (r *LocalBackend) OAuthLoginCallback(ctx context.Context, oAuthToken string) (*account.UserData, error) {
	return r.accountClient.OAuthLoginCallback(ctx, oAuthToken)
}

func (r *LocalBackend) OAuthLoginURL(ctx context.Context, provider string) (string, error) {
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

func (r *LocalBackend) StripeBillingPortalURL(ctx context.Context) (string, error) {
	return r.accountClient.StripeBillingPortalURL(ctx,
		common.GetProServerURL(), settings.GetString(settings.UserIDKey), settings.GetString(settings.TokenKey),
	)
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
