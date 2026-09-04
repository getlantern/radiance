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
	"github.com/getlantern/radiance/unbounded"
	"github.com/getlantern/radiance/vpn"

	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/sagernet/sing-box/option"
)

const tracerName = "github.com/getlantern/radiance/backend"

// LocalBackend ties all the core functionality of Radiance together. It manages the configuration,
// servers, VPN connection, account management, issue reporting, and telemetry for the application.
//
// A nil LocalBackend is valid for reporting issues.
type LocalBackend struct {
	ctx    context.Context
	cancel context.CancelFunc

	confHandler   *config.ConfigHandler
	issueReporter *issue.IssueReporter
	accountClient *account.Client

	srvManager     *servers.Manager
	vpnClient      *vpn.VPNClient
	splitTunnelMgr *vpn.SplitTunnel
	sessionHistory *vpn.SessionHistory

	peerClient   peerController
	peerToggleMu sync.Mutex
	peerWG       sync.WaitGroup

	shutdownFuncs []func() error
	closeOnce     sync.Once

	// Rejects overlapping ConnectVPN calls (TryLock) so a burst can't each
	// drive a full tunnel bring-up.
	connectMu sync.Mutex

	deviceID string

	telemetryCfgSub *events.Subscription[config.NewConfigEvent]

	// reused across reconnects; fully released only when telemetry shuts down.
	connObserver    vpn.ConnObserver
	stopConnMetrics context.CancelFunc
	connMetricsMu   sync.Mutex

	dataCapCh   chan *account.DataCapInfo // latest datacap update; nil when stream not running
	stopDataCap context.CancelFunc
	dataCapMu   sync.Mutex

	stopSelectionHistoryListener context.CancelFunc
	selectionHistoryMu           sync.Mutex
	selectionReporter            *selectionReporter

	exhaustionGate exhaustionGate
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
	// Invariant: a user must always be able to construct a backend and report an
	// issue, even when on-disk state is unreadable or incompatible (e.g. after a
	// downgrade). Failures loading the server manager, split tunnel, and config
	// are logged and degraded, never returned. The only fatal path is
	// common.Init, which fails only when the data directory or settings file
	// can't be created or read — i.e. the app genuinely cannot run.

	// Must run before common.Init: it reads RADIANCE_VERSION once and
	// freezes it, so a later Setenv is ignored by the header-fill path.
	var envOverrideErrs error
	for k, v := range opts.EnvOverrides {
		if err := os.Setenv(k, v); err != nil {
			envOverrideErrs = errors.Join(envOverrideErrs, fmt.Errorf("apply env override %q: %w", k, err))
		}
	}
	if err := common.Init(opts.DataDir, opts.LogDir, opts.LogLevel); err != nil {
		return nil, fmt.Errorf("failed to initialize common components: %w", err)
	}
	if envOverrideErrs != nil {
		slog.Warn("Failed to apply some env overrides", "error", envOverrideErrs)
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
		// The device ID is owned by the native caller and shared to the extension
		// through persisted settings so both use the same ID. Unlike the desktop
		// branch, don't generate one here — log and continue if it's absent.
		switch {
		case opts.DeviceID != "":
			platformDeviceID = opts.DeviceID
		case settings.Exists(settings.DeviceIDKey):
			if platformDeviceID = settings.GetString(settings.DeviceIDKey); platformDeviceID == "" {
				slog.Warn("No device ID was found")
			}
		default:
			slog.Warn("No device ID was found")
		}
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
		slog.Error("Loading server manager", "error", err)
	}

	splitTunnelMgr, err := vpn.NewSplitTunnelHandler(
		dataDir, slog.Default().With("service", "split_tunnel"),
	)
	if err != nil {
		slog.Error("Loading split tunnel handler", "error", err)
	}
	// Empty filters do not persist the invert flag, so reload can reset the
	// loaded policy. Re-apply the persisted setting as the source of truth.
	if err := splitTunnelMgr.SetPolicy(vpn.SplitTunnelPolicy(settings.GetString(settings.SplitTunnelPolicyKey))); err != nil {
		slog.Warn("Reconciling split-tunnel policy", "error", err)
	}

	vpnClient := vpn.NewVPNClient(dataDir, slog.Default().With("service", "vpn"), opts.PlatformInterface)

	// Degraded, not fatal, per the invariant above: a nil peerClient only
	// disables Share My Connection, and must not cost the user their ability
	// to report an issue. applyPeerShare and PeerStatus handle nil.
	peerClient, err := newPeerClient(platformDeviceID)
	if err != nil {
		slog.Error("Loading peer client", "error", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	cOpts := config.Options{
		DataPath:      dataDir,
		Locale:        opts.Locale,
		AccountClient: accountClient,
		HTTPClient:    kindling.HTTPClient(),
		Logger:        slog.Default().With("service", "config_handler"),
	}
	r := &LocalBackend{
		ctx:               ctx,
		cancel:            cancel,
		issueReporter:     issue.NewIssueReporter(kindling.HTTPClient()),
		selectionReporter: newSelectionReporter(kindling.HTTPClient()),
		accountClient:     accountClient,
		confHandler:       config.NewConfigHandler(ctx, cOpts),
		srvManager:        svrMgr,
		vpnClient:         vpnClient,
		splitTunnelMgr:    splitTunnelMgr,
		peerClient:        peerClient,
		shutdownFuncs: []func() error{
			telemetry.Close, kindling.Close,
		},
		closeOnce: sync.Once{},
		deviceID:  platformDeviceID,
		dataCapCh: make(chan *account.DataCapInfo, 1),
	}
	r.sessionHistory = vpn.NewSessionHistory(slog.Default().With("service", "session_history"), r.sessionInfo())
	r.shutdownFuncs = append(r.shutdownFuncs, func() error { r.sessionHistory.Close(); return nil })
	r.clearSelectedIfMissing()
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
			Methods:      publicip.DefaultMethods(),
		})
		cancel()
		if err != nil {
			slog.Warn("Failed to get public IP", "error", err)
		} else {
			common.SetPublicIP(result.IP.String())
			// IP intentionally omitted — Lantern users in censored regions
			// can't safely have their public IP in routinely-collected
			// client logs. Confidence + sources are enough for operator
			// triage; the actual IP is correlated server-side via traces.
			slog.Info("Detected public IP", "confidence", result.Confidence, "sources", result.Sources)
		}
	}()

	if settings.GetBool(settings.TelemetryKey) {
		if err := r.startTelemetry(); err != nil {
			slog.Error("Failed to start telemetry", "error", err)
		}
	}
	r.startVPNStatusListeners()
	r.startAutoSelectedListener()
	r.startSessionAutoSelectListener()

	r.resumePeerShareIfEnabled()

	// Wire the broflake / Unbounded widget proxy lifecycle to config
	// updates. This single subscription handles all three start/stop
	// triggers (local toggle, server feature flag, server-supplied
	// config); InitSubscription is sync.Once-guarded so a future Start
	// retry after Close won't double-subscribe.
	//
	// Seed with the already-cached config (loaded from disk before
	// Start runs) so an opted-in user auto-starts the widget on
	// launch instead of waiting for the next config refresh.
	cachedCfg, _ := r.confHandler.GetConfig()
	unbounded.InitSubscription(cachedCfg)

	// The server derives the country from the client IP, so it's stable for the
	// session: react once to record it for issue reports.
	events.SubscribeOnce(func(evt config.NewConfigEvent) {
		setCountryCodeFromConfig(evt.New)
	})
	events.SubscribeContext(r.ctx, func(evt config.NewConfigEvent) {
		r.applyConfig(evt.New)
		go r.prewarmOfflineURLTests("config update")
	})
	if r.applyCurrentConfig() {
		go r.prewarmOfflineURLTests("cached config")
	}
	r.confHandler.Start()
}

// applyCurrentConfig applies any config already loaded from disk before the
// fetch loop has a chance to refresh it.
func (r *LocalBackend) applyCurrentConfig() bool {
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		return false
	}
	setCountryCodeFromConfig(cfg)
	r.applyConfig(cfg)
	return true
}

// prewarmOfflineURLTests records reachability history for the not-yet-connected
// tunnel path and treats connected-tunnel races as harmless.
func (r *LocalBackend) prewarmOfflineURLTests(source string) {
	if err := r.RunOfflineURLTests(); err != nil && !errors.Is(err, vpn.ErrTunnelAlreadyConnected) {
		// ErrTunnelAlreadyConnected is expected while the VPN is up:
		// updateServers already pushed the new outbounds into the live
		// tunnel via UpdateOutbounds, so the offline pre-warm, which
		// targets the not-yet-connected case, would duplicate work and
		// conflict with the live auto-select group.
		slog.Error("Failed to run offline URL tests", "source", source, "error", err)
	}
}

// applyConfig updates the runtime server state from a config snapshot.
// Startup-loaded cached configs and freshly fetched configs both use this path.
func (r *LocalBackend) applyConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	list := serverListFromConfig(cfg)
	if len(cfg.BanditURLOverrides) > 0 {
		if ctx, ok := traces.ExtractBanditTraceContext(cfg.BanditURLOverrides); ok {
			// Link this marker span to the API's bandit trace so config receipt
			// and the per-outbound callback stay visible in one distributed trace.
			_, span := otel.Tracer(tracerName).Start(ctx, "radiance.config_received",
				trace.WithAttributes(
					attribute.Int("bandit.override_count", len(cfg.BanditURLOverrides)),
					attribute.Int("bandit.outbound_count", len(cfg.Options.Outbounds)),
				),
			)
			span.End()
		}
	}
	if err := r.updateServers(list); err != nil {
		slog.Error("updating servers in manager", "error", err)
	}
	if err := r.vpnClient.UpdateNonSelectableOutbounds(nonSelectableOutboundsFromConfig(cfg)); err != nil &&
		!errors.Is(err, vpn.ErrTunnelNotConnected) {
		slog.Error("updating non-selectable outbounds", "error", err)
	}
}

// setCountryCodeFromConfig stores the config country for diagnostics unless
// an explicit country override is active.
func setCountryCodeFromConfig(cfg *config.Config) {
	if env.GetString(env.Country) != "" || cfg == nil || cfg.Country == "" {
		return
	}
	if err := settings.Set(settings.CountryCodeKey, cfg.Country); err != nil {
		slog.Error("failed to set country code in settings", "error", err)
	}
	slog.Info("Set country code from config", "country_code", cfg.Country)
}

// serverListFromConfig converts config outbounds and endpoints into managed
// Lantern servers while preserving location and bandit URL metadata.
func serverListFromConfig(cfg *config.Config) servers.ServerList {
	nonSelectable := nonSelectableSet(cfg)
	srvs := make([]*servers.Server, 0, len(cfg.Options.Outbounds)+len(cfg.Options.Endpoints))
	addSvr := func(tag, typ string, opts any, loc *C.ServerLocation) {
		// Non-selectable tags are merged into the box for dialing but must never
		// become managed servers, or they surface in the server list, get probed by
		// offline URL tests, and join the auto-select group on live update.
		if _, ok := nonSelectable[tag]; ok {
			return
		}
		s := &servers.Server{
			Tag: tag, Type: typ, IsLantern: true, Options: opts,
		}
		if loc != nil {
			s.Location = *loc
		}
		srvs = append(srvs, s)
	}
	for _, out := range cfg.Options.Outbounds {
		addSvr(out.Tag, out.Type, out, cfg.OutboundLocations[out.Tag])
	}
	for _, ep := range cfg.Options.Endpoints {
		addSvr(ep.Tag, ep.Type, ep, cfg.OutboundLocations[ep.Tag])
	}
	return servers.ServerList{Servers: srvs, URLOverrides: cfg.BanditURLOverrides}
}

func nonSelectableSet(cfg *config.Config) map[string]struct{} {
	set := make(map[string]struct{}, len(cfg.NonSelectableOutbounds))
	for _, tag := range cfg.NonSelectableOutbounds {
		set[tag] = struct{}{}
	}
	return set
}

// nonSelectableOutboundsFromConfig returns the config's non-selectable outbounds for
// the tunnel to instantiate off the managed-server path. Endpoints are excluded: a
// non-selectable endpoint stays build-time only.
func nonSelectableOutboundsFromConfig(cfg *config.Config) servers.ServerList {
	nonSelectable := nonSelectableSet(cfg)
	srvs := make([]*servers.Server, 0, len(nonSelectable))
	for _, out := range cfg.Options.Outbounds {
		if _, ok := nonSelectable[out.Tag]; !ok {
			continue
		}
		srvs = append(srvs, &servers.Server{Tag: out.Tag, Type: out.Type, IsLantern: true, Options: out})
	}
	return servers.ServerList{Servers: srvs}
}

func (r *LocalBackend) Close() {
	r.closeOnce.Do(func() {
		slog.Debug("Closing Radiance")
		r.closePeerClient()
		// unbounded.start spawns its worker on a context.Background-
		// derived ctx (it has to outlive any single NewConfigEvent),
		// so Close has to explicitly tell it to shut down — otherwise
		// the broflake widget goroutine survives backend close and
		// leaks until process exit. Use a fresh ctx so a cancelled
		// shutdown path doesn't skip the Stop.
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := unbounded.Stop(stopCtx); err != nil {
			slog.Warn("unbounded stop on backend close returned error", "error", err)
		}
		cancel()
		// vpnClient is always set in production via NewLocalBackend, but
		// peer-focused unit tests construct partial LocalBackends without
		// one. Guard the call so Close stays robust under those paths
		// rather than panicking in DisconnectVPN.
		if r.vpnClient != nil {
			if err := r.DisconnectVPN(); err != nil {
				slog.Error("Failed to disconnect VPN on shutdown", "error", err)
			}
		}
		r.cancel() // cancels context, unsubscribes all event listeners and stops child goroutines
		for _, shutdown := range r.shutdownFuncs {
			if err := shutdown(); err != nil {
				slog.Error("Failed to shutdown", "error", err)
			}
		}
	})
}

func (r *LocalBackend) startVPNStatusListeners() {
	events.SubscribeContext(r.ctx, func(evt vpn.StatusUpdateEvent) {
		r.updateConnMetrics(evt.Status)
	})
	events.SubscribeContext(r.ctx, func(evt vpn.StatusUpdateEvent) {
		r.updateDataCapStream(evt.Status)
	})
	events.SubscribeContext(r.ctx, func(evt vpn.StatusUpdateEvent) {
		r.updateSelectionHistoryListener(evt.Status)
	})
	events.SubscribeContext(r.ctx, func(vpn.ExhaustionEvent) {
		r.refetchOnExhaustion()
	})
	events.SubscribeContext(r.ctx, func(evt vpn.NetworkEvent) {
		switch evt.EventType {
		case vpn.NetworkEventPaused:
			kindling.Pause()
		case vpn.NetworkEventWake:
			kindling.Resume()
		}
	})
	events.SubscribeContext(r.ctx, func(evt vpn.StatusUpdateEvent) {
		switch evt.Status {
		case vpn.Disconnected, vpn.ErrorStatus, vpn.Restarting:
			r.clearSelectedIfMissing()
		}
	})
}

func (r *LocalBackend) sessionInfo() vpn.SessionInfo {
	return vpn.SessionInfo{
		Status: r.vpnClient.Status,
		SelectedServer: func() (tag, city, country string) {
			server, _, err := r.SelectedServer()
			if err != nil || server == nil {
				return "", "", ""
			}
			return server.Tag, server.Location.City, server.Location.Country
		},
		Bytes: r.vpnClient.Bytes,
	}
}

func (r *LocalBackend) Sessions(limit int) []vpn.Session {
	return r.sessionHistory.Sessions(limit)
}

//////////////////
// Issue Report //
//////////////////

type issueReportMetadata struct {
	country            string
	deviceID           string
	reporter           *issue.IssueReporter
	splitTunnelEnabled bool
}

// buildIssueReportMetadata gathers the backend state needed to file an issue
// report. It is safe to call with a nil or partially initialized backend.
func (r *LocalBackend) buildIssueReportMetadata() issueReportMetadata {
	meta := issueReportMetadata{
		country:            settings.GetString(settings.CountryCodeKey),
		deviceID:           settings.GetString(settings.DeviceIDKey),
		splitTunnelEnabled: settings.GetBool(settings.SplitTunnelKey),
	}

	if r == nil {
		meta.reporter = issue.NewIssueReporter(kindling.HTTPClient())
		return meta
	}
	if r.issueReporter != nil {
		meta.reporter = r.issueReporter
	} else {
		meta.reporter = issue.NewIssueReporter(kindling.HTTPClient())
	}
	if r.deviceID != "" {
		meta.deviceID = r.deviceID
	}

	if r.confHandler != nil {
		if cfg, err := r.confHandler.GetConfig(); err != nil {
			slog.Warn("failed to get config", "error", err)
		} else {
			if cfg.Country != "" {
				meta.country = cfg.Country
			}
		}
	}

	if r.splitTunnelMgr != nil {
		meta.splitTunnelEnabled = r.splitTunnelMgr.IsEnabled()
	}

	return meta
}

// ReportIssue sends an issue report with current metadata and diagnostic attachments.
//
// ReportIssue is safe to call with a nil receiver; in that case it uses settings-backed metadata.
func (r *LocalBackend) ReportIssue(
	ctx context.Context,
	issueType issue.IssueType,
	description, email string,
	additionalAttachments []string,
	attachments []*issue.Attachment,
) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "report_issue")
	defer span.End()

	meta := r.buildIssueReportMetadata()

	attachmentPaths := baseIssueAttachments()
	if meta.splitTunnelEnabled {
		attachmentPaths = append(attachmentPaths, filepath.Join(settings.GetString(settings.DataPathKey), internal.SplitTunnelFileName))
	}
	attachmentPaths = append(attachmentPaths, additionalAttachments...)

	report := issue.IssueReport{
		Type:                  issueType,
		Description:           description,
		Email:                 email,
		CountryCode:           meta.country,
		DeviceID:              meta.deviceID,
		UserID:                settings.GetString(settings.UserIDKey),
		SubscriptionLevel:     settings.GetString(settings.UserLevelKey),
		Locale:                settings.GetString(settings.LocaleKey),
		Attachments:           attachments,
		AdditionalAttachments: attachmentPaths,
	}
	if err := meta.reporter.Report(ctx, report); err != nil {
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
	files := []string{
		filepath.Join(logPath, internal.CrashLogFileName),
		filepath.Join(dataPath, internal.ConfigFileName),
		filepath.Join(dataPath, internal.ServersFileName),
		filepath.Join(dataPath, internal.DebugBoxOptionsFileName),
	}
	memdump := filepath.Join(logPath, internal.MemoryDumpFileName)
	if _, err := os.Stat(memdump); err == nil {
		// put memory dump first in the list so it's prioritized.
		files = append([]string{memdump}, files...)
	}
	return files
}

/////////////////
//  Settings   //
/////////////////

// UpdateConfig forces an immediate fetch of the latest configuration. It returns
// [config.ErrConfigFetchDisabled] if config fetching is disabled in settings.
func (r *LocalBackend) UpdateConfig() error {
	return r.confHandler.Fetch()
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
	// Reject an invalid split-tunnel policy before persisting, so settings.json
	// can't hold a value the runtime would silently fall back to exclude for.
	if v, ok := diff[settings.SplitTunnelPolicyKey]; ok {
		if p := vpn.SplitTunnelPolicy(fmt.Sprintf("%v", v)); !p.Valid() {
			return fmt.Errorf("invalid %s: %v", settings.SplitTunnelPolicyKey, v)
		}
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
	//
	// settings.Patch above already persisted the whole diff, so an early return
	// on a handler error would leave a persisted key that no runtime state
	// matches — the divergence applyPeerShare's rollback exists to prevent. Run
	// every handler and join their errors so the caller sees write failures.
	var errs error
	if _, ok := diff[settings.SplitTunnelKey]; ok {
		if err := r.splitTunnelMgr.SetEnabled(settings.GetBool(settings.SplitTunnelKey)); err != nil {
			errs = errors.Join(errs, fmt.Errorf("set split-tunnel enabled: %w", err))
		}
	}
	if _, ok := diff[settings.SplitTunnelPolicyKey]; ok {
		if err := r.splitTunnelMgr.SetPolicy(vpn.SplitTunnelPolicy(settings.GetString(settings.SplitTunnelPolicyKey))); err != nil {
			errs = errors.Join(errs, fmt.Errorf("set split-tunnel policy: %w", err))
		}
	}
	if err := r.maybeRestartVPN(diff); err != nil {
		errs = errors.Join(errs, err)
	}
	if _, ok := diff[settings.PeerShareEnabledKey]; ok {
		if err := r.applyPeerShare(settings.GetBool(settings.PeerShareEnabledKey)); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	// Drive the Unbounded widget proxy off the toggle change immediately
	// rather than waiting for the next NewConfigEvent to re-evaluate.
	// settings.Patch above has already persisted the new value, so go
	// straight to Apply() — SetEnabled would short-circuit on the
	// already-matching persisted value and never re-evaluate the
	// manager. Apply re-checks the three-condition predicate against
	// the cached server-side state and starts or stops accordingly.
	if _, ok := diff[settings.UnboundedKey]; ok {
		if err := unbounded.Apply(); err != nil {
			slog.Warn("unbounded apply failed", "error", err)
		}
	}
	return errs
}

// maybeRestartVPN restarts the VPN connection if either the ad block or smart routing settings
// were changed and the VPN is currently connected. Returns an error if the VPN restart fails;
// otherwise returns nil.
func (r *LocalBackend) maybeRestartVPN(updates settings.Settings) error {
	_, adBlockChanged := updates[settings.AdBlockKey]
	_, smartRoutingChanged := updates[settings.SmartRoutingKey]
	if (adBlockChanged || smartRoutingChanged) && r.vpnClient.Status() == vpn.Connected {
		slog.Info("Restarting VPN to apply new settings", "ad_block_changed", adBlockChanged, "smart_routing_changed", smartRoutingChanged)
		if err := r.RestartVPN(); err != nil {
			return fmt.Errorf(
				"failed to restart VPN (ad_block_changed=%v, smart_routing_changed=%v): %w",
				adBlockChanged, smartRoutingChanged, err,
			)
		}
	}
	return nil
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
	r.teardownConnMetrics()
	telemetry.Close()
}

func (r *LocalBackend) updateConnMetrics(status vpn.VPNStatus) {
	if !settings.GetBool(settings.TelemetryKey) {
		return
	}
	r.connMetricsMu.Lock()
	defer r.connMetricsMu.Unlock()

	if status != vpn.Connected {
		r.vpnClient.SetConnObserver(nil)
		return
	}
	if !r.ensureConnMetricsObserverLocked() {
		return
	}
	r.vpnClient.SetConnObserver(r.connObserver)
}

func (r *LocalBackend) ensureConnMetricsObserverLocked() bool {
	if r.connObserver != nil {
		return true
	}

	ctx, cancel := context.WithCancel(r.ctx)
	observer, err := telemetry.StartConnectionMetrics(ctx, r.vpnClient.ActiveConnectionCount)
	if err != nil {
		cancel()
		slog.Warn("Failed to start connection metrics collection", "error", err)
		return false
	}

	r.connObserver = observer
	r.stopConnMetrics = cancel
	return true
}

// teardownConnMetrics fully detaches and releases the connection metrics observer.
func (r *LocalBackend) teardownConnMetrics() {
	r.connMetricsMu.Lock()
	defer r.connMetricsMu.Unlock()
	if r.vpnClient != nil {
		r.vpnClient.SetConnObserver(nil)
	}
	if r.stopConnMetrics != nil {
		r.stopConnMetrics()
		r.stopConnMetrics = nil
	}
	r.connObserver = nil
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
		r.clearSelectedIfMissing()
		if err := r.vpnClient.RemoveOutbounds(removedTags); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
			return fmt.Errorf("failed to remove outbounds: %w", err)
		}
	}
	return nil
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

func (r *LocalBackend) AddServersByJSON(config string) ([]string, error) {
	list, err := r.srvManager.AddServersByJSON(r.ctx, []byte(config))
	if err != nil {
		return nil, fmt.Errorf("failed to add servers by JSON: %w", err)
	}
	if err := r.vpnClient.AddOutbounds(*list); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		return nil, fmt.Errorf("failed to add outbounds to VPN client: %w", err)
	}
	return list.Tags(), nil
}

func (r *LocalBackend) AddServersByURL(urls []string, skipCertVerification bool) ([]string, error) {
	list, err := r.srvManager.AddServersByURL(r.ctx, urls, skipCertVerification)
	if err != nil {
		return nil, fmt.Errorf("failed to add servers by URL: %w", err)
	}
	if err := r.vpnClient.AddOutbounds(*list); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		return nil, fmt.Errorf("failed to add outbounds to VPN client: %w", err)
	}
	return list.Tags(), nil
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

// maxRetainedLanternServers caps the number of working Lantern servers retained
// across config updates.
const maxRetainedLanternServers = 60

func (r *LocalBackend) updateServers(list servers.ServerList) error {
	existing := r.srvManager.AllServers()
	existingTags := serverTagSet(existing)
	list.Servers = slices.DeleteFunc(list.Servers, func(srv *servers.Server) bool {
		_, exists := existingTags[srv.Tag]
		return exists
	})

	tagsToEvict := lanternServersToEvict(existing, len(list.Servers), maxRetainedLanternServers)

	if len(tagsToEvict) > 0 {
		slog.Debug(
			"Evicting retained Lantern servers to make room for new config batch",
			"count", len(tagsToEvict),
			"tags", tagsToEvict,
		)
		if _, err := r.srvManager.RemoveServers(tagsToEvict); err != nil {
			return fmt.Errorf("remove retained Lantern servers: %w", err)
		}
	}

	slog.Debug(
		"Adding new Lantern servers from config update",
		"count", len(list.Servers),
		"tags", slices.Collect(maps.Keys(serverTagSet(list.Servers))),
	)
	if err := r.srvManager.AddServers(list, false); err != nil {
		return fmt.Errorf("add Lantern servers: %w", err)
	}
	// updateOutbounds evicts any outbound absent from the list; include all
	// servers so user-added outbounds aren't removed on a Lantern config update.
	allList := servers.ServerList{Servers: r.srvManager.AllServers(), URLOverrides: list.URLOverrides}
	if err := r.vpnClient.UpdateOutbounds(allList); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		return fmt.Errorf("failed to update VPN outbounds: %w", err)
	}
	if r.vpnClient.Status() != vpn.Connected {
		r.clearSelectedIfMissing()
	}
	return nil
}

func serverTagSet(list []*servers.Server) map[string]struct{} {
	tags := make(map[string]struct{}, len(list))
	for _, srv := range list {
		tags[srv.Tag] = struct{}{}
	}
	return tags
}

// lanternServersToEvict returns the Lantern server tags to remove before the
// next config batch is added. Hard-demoted servers are always evicted so a
// later re-offer is treated as a fresh candidate and re-probed. Remaining
// candidates are evicted oldest-first by SelectionHistory.UpdatedAt; missing
// history sorts oldest.
func lanternServersToEvict(
	existing []*servers.Server,
	incomingCount, limit int,
) []string {
	tagsToEvict := make([]string, 0)
	retentionCandidates := make([]*servers.Server, 0, len(existing))

	for _, srv := range existing {
		if !srv.IsLantern {
			continue
		}
		// Always evict hard-demoted servers.
		if isHardDemoted(srv) {
			tagsToEvict = append(tagsToEvict, srv.Tag)
			continue
		}
		retentionCandidates = append(retentionCandidates, srv)
	}

	retentionBudget := max(limit-incomingCount, 0)
	if len(retentionCandidates) <= retentionBudget {
		return tagsToEvict
	}

	slices.SortFunc(retentionCandidates, compareSelectionAge)

	overflow := len(retentionCandidates) - retentionBudget
	for _, srv := range retentionCandidates[:overflow] {
		tagsToEvict = append(tagsToEvict, srv.Tag)
	}

	return tagsToEvict
}

func isHardDemoted(srv *servers.Server) bool {
	return srv.SelectionHistory != nil && srv.SelectionHistory.HardDemoted
}

func compareSelectionAge(a, b *servers.Server) int {
	return selectionUpdatedAt(a).Compare(selectionUpdatedAt(b))
}

func selectionUpdatedAt(srv *servers.Server) time.Time {
	if srv.SelectionHistory == nil {
		return time.Time{}
	}
	return srv.SelectionHistory.UpdatedAt
}

// clearSelectedIfMissing reverts the persisted selection to auto-select when
// the selected server is no longer present in the manager.
func (r *LocalBackend) clearSelectedIfMissing() {
	var selected servers.Server
	if err := settings.GetStruct(settings.SelectedServerKey, &selected); err != nil {
		return
	}
	if _, found := r.srvManager.GetServerByTag(selected.Tag); found {
		return
	}
	// Persist before notifying the VPN client so the auto-select choice
	// survives even if the tunnel isn't running to accept the switch.
	r.persistSelection(vpn.AutoSelectTag)
	if err := r.vpnClient.SelectServer(vpn.AutoSelectTag); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		slog.Warn("Failed to switch to auto-select after selected server was removed", "error", err)
	}
}

const selectionHistoryFlushInterval = 5 * time.Second

func (r *LocalBackend) updateSelectionHistoryListener(status vpn.VPNStatus) {
	r.selectionHistoryMu.Lock()
	defer r.selectionHistoryMu.Unlock()
	switch status {
	case vpn.Connected:
		if r.stopSelectionHistoryListener != nil {
			r.stopSelectionHistoryListener()
			r.stopSelectionHistoryListener = nil
		}
		storage := r.vpnClient.HistoryStorage()
		if storage == nil {
			return
		}
		ctx, cancel := context.WithCancel(r.ctx)
		r.stopSelectionHistoryListener = cancel
		hook := make(chan struct{}, 1)
		storage.SetHook(func(string) {
			// Per-tag granularity isn't useful — flushSelectionHistory
			// iterates every server. Non-blocking send so storage
			// writes never block on a slow flush.
			select {
			case hook <- struct{}{}:
			default:
			}
		})
		go r.runSelectionHistoryListener(ctx, storage, hook)
		if r.selectionReportInterval() > 0 {
			r.selectionReporter.reset()
			go r.runSelectionReporter(ctx, storage)
		}
		slog.Debug("Started selection history listener")
	case vpn.Disconnected, vpn.ErrorStatus:
		if r.stopSelectionHistoryListener != nil {
			r.stopSelectionHistoryListener()
			r.stopSelectionHistoryListener = nil
			slog.Debug("Stopped selection history listener")
		}
	}
}

// runSelectionHistoryListener coalesces per-result hook notifications into a periodic flush so the
// servers file isn't rewritten for each parallel probe completion. A final flush runs on shutdown so
// any results that arrived since the last tick are persisted.
func (r *LocalBackend) runSelectionHistoryListener(ctx context.Context, storage vpn.AutoSelectHistoryStorage, hook <-chan struct{}) {
	ticker := time.NewTicker(selectionHistoryFlushInterval)
	defer ticker.Stop()
	dirty := true // start dirty so a seed/result present before the listener started still gets persisted
	for {
		select {
		case <-ctx.Done():
			if dirty {
				r.flushSelectionHistory(storage)
			}
			return
		case <-hook:
			dirty = true
		case <-ticker.C:
			if dirty {
				r.flushSelectionHistory(storage)
				dirty = false
			}
		}
	}
}

func (r *LocalBackend) flushSelectionHistory(storage vpn.AutoSelectHistoryStorage) {
	history := r.collectSelectionHistory(storage)
	if len(history) == 0 {
		return
	}

	if err := r.srvManager.UpdateSelectionHistory(history); err != nil {
		slog.Warn("Failed to persist selection history", "error", err)
	}
}

func (r *LocalBackend) collectSelectionHistory(storage vpn.AutoSelectHistoryStorage) map[string]servers.SelectionHistory {
	history := make(map[string]servers.SelectionHistory)
	for _, srv := range r.srvManager.AllServers() {
		if h := storage.Load(srv.Tag); h != nil {
			history[srv.Tag] = *h
		}
	}
	return history
}

// selectionReportInterval is the server-configured report cadence. Zero is
// returned if reporting is disabled or the config is unavailable.
func (r *LocalBackend) selectionReportInterval() time.Duration {
	cfg, _ := r.confHandler.GetConfig()
	if cfg == nil || cfg.RouteSelectionReportIntervalSeconds <= 0 {
		return 0
	}

	return time.Duration(cfg.RouteSelectionReportIntervalSeconds) * time.Second
}

func (r *LocalBackend) runSelectionReporter(ctx context.Context, storage vpn.AutoSelectHistoryStorage) {
	runReportLoop(ctx, r.selectionReportInterval, func() {
		r.reportSelectionHistory(ctx, storage)
	})
}

// runReportLoop calls report once per interval() until the context is canceled
// or interval() returns a non-positive duration. The interval is re-derived each
// cycle so a changed cadence takes effect the next tick and a dropped interval
// stops the loop within one cycle.
func runReportLoop(ctx context.Context, interval func() time.Duration, report func()) {
	for {
		wait := interval()
		if wait <= 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			report()
		}
	}
}

func (r *LocalBackend) reportSelectionHistory(ctx context.Context, storage vpn.AutoSelectHistoryStorage) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "report_selection_history")
	defer span.End()

	cfg, _ := r.confHandler.GetConfig()
	if cfg == nil || len(cfg.BanditReportTokens) == 0 {
		return
	}
	snapshot := r.collectSelectionHistory(storage)
	if len(snapshot) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, selectionReportTimeout)
	defer cancel()
	if err := r.selectionReporter.report(ctx, snapshot, cfg.BanditReportTokens); err != nil {
		slog.Warn("Failed to report selection history", "error", err)
		span.RecordError(err)
	}
}

/////////////////
//     VPN     //
/////////////////

func (r *LocalBackend) VPNStatus() vpn.VPNStatus {
	return r.vpnClient.Status()
}

// ErrConnectInProgress signals that a connect attempt is already running. It is
// benign, not a failure: bursts of connect requests (e.g. rapid taps while every
// attempt is blocked in a censored network) must not each drive a full tunnel
// bring-up.
var ErrConnectInProgress = errors.New("connect already in progress")

func (r *LocalBackend) ConnectVPN(ctx context.Context, tag string) error {
	// Shed overlapping connects before getBoxOptions: it assembles the full
	// outbound set, and without this each queued request also stacks a full
	// sing-box bring-up behind vpnClient's mutex.
	if !r.connectMu.TryLock() {
		return ErrConnectInProgress
	}
	defer r.connectMu.Unlock()

	if tag == "" {
		tag = vpn.AutoSelectTag
	}
	if err := r.awaitConnectable(ctx, tag); err != nil {
		return err
	}
	if tag != vpn.AutoSelectTag {
		if _, found := r.srvManager.GetServerByTag(tag); !found {
			return fmt.Errorf("no server found with tag %s", tag)
		}
	}
	bOptions := r.getBoxOptions()
	bOptions.InitialServer = tag
	if err := r.vpnClient.Connect(ctx, bOptions); err != nil {
		return fmt.Errorf("failed to connect VPN: %w", err)
	}
	r.persistSelection(tag)
	return nil
}

// awaitConnectable blocks until the connect has something to dial, or ctx is
// done. Connecting with neither a config nor a server of the user's own
// produces "no outbounds or endpoints found", and on mobile the process that
// reports that failure is the one hosting the config fetch it needed, so
// failing fast deadlocks the first run.
func (r *LocalBackend) awaitConnectable(ctx context.Context, tag string) error {
	if r.connectable(tag) {
		return nil
	}
	slog.Info("Waiting for a config before connecting", "tag", tag)
	ready := make(chan struct{})
	// Subscribe, not SubscribeOnce: this unsubscribes on return anyway, and
	// SubscribeUntil's self-referential `sub` capture is read from the callback
	// goroutine without synchronization.
	//
	// Emit runs each callback on its own goroutine, so configs landing together
	// can both reach this. Closing twice would panic.
	var closeOnce sync.Once
	sub := events.Subscribe(func(config.NewConfigEvent) {
		closeOnce.Do(func() { close(ready) })
	})
	defer sub.Unsubscribe()
	// A config can land between the check above and the subscription.
	if r.connectable(tag) {
		return nil
	}
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("no config to connect with: %w", ctx.Err())
	}
}

// connectable reports whether a connect can proceed right now: either a config
// has been loaded, or the user has a server of their own to dial without one.
func (r *LocalBackend) connectable(tag string) bool {
	if _, err := r.confHandler.GetConfig(); err == nil {
		return true
	}
	if tag != vpn.AutoSelectTag {
		_, found := r.srvManager.GetServerByTag(tag)
		return found
	}
	return len(r.srvManager.AllServers()) > 0
}

func (r *LocalBackend) getBoxOptions() vpn.BoxOptions {
	// ignore error, we can still connect with default options if config is not available for some reason
	cfg, _ := r.confHandler.GetConfig()
	bOptions := vpn.BoxOptions{
		BasePath: settings.GetString(settings.DataPathKey),
	}
	if cfg != nil {
		bOptions.Options = cfg.Options
		bOptions.NonSelectableOutbounds = cfg.NonSelectableOutbounds
		bOptions.BanditURLOverrides = cfg.BanditURLOverrides
		if settings.GetBool(settings.SmartRoutingKey) {
			bOptions.SmartRouting = cfg.SmartRouting
		}
		if settings.GetBool(settings.AdBlockKey) {
			bOptions.AdBlock = cfg.AdBlock
		}
	}
	managedServers := r.srvManager.AllServers()
	appendManagedServerOptions(&bOptions.Options, managedServers)
	bOptions.LanternServerTags = lanternServerTags(cfg, managedServers)

	seed := make(map[string]lbA.TagHistory)
	for _, srv := range managedServers {
		if srv.SelectionHistory != nil {
			seed[srv.Tag] = *srv.SelectionHistory
		}
	}
	if len(seed) > 0 {
		bOptions.SelectionHistorySeed = seed
	}
	return bOptions
}

// lanternServerTags collects the tags of the Lantern servers in cfg and
// managed, to seed the client-context injector's match bounds. Every cfg
// outbound and endpoint is a Lantern server; managed servers carry the flag
// explicitly.
func lanternServerTags(cfg *config.Config, managed []*servers.Server) []string {
	seen := make(map[string]struct{})
	var tags []string
	add := func(tag string) {
		if tag == "" {
			return
		}
		if _, ok := seen[tag]; ok {
			return
		}
		seen[tag] = struct{}{}
		tags = append(tags, tag)
	}
	if cfg != nil {
		for _, out := range cfg.Options.Outbounds {
			add(out.Tag)
		}
		for _, ep := range cfg.Options.Endpoints {
			add(ep.Tag)
		}
	}
	for _, srv := range managed {
		if srv.IsLantern {
			add(srv.Tag)
		}
	}
	return tags
}

// appendManagedServerOptions adds server-manager options that are missing from
// the current config options. Config options remain authoritative for duplicate
// tags, but retained Lantern servers and user servers stay connectable on cold
// tunnel start.
func appendManagedServerOptions(options *option.Options, managed []*servers.Server) {
	existingTags := optionTagSet(*options)
	for _, srv := range managed {
		switch opts := srv.Options.(type) {
		case option.Outbound:
			tag := opts.Tag
			if tag == "" {
				continue
			}
			if _, exists := existingTags[tag]; exists {
				continue
			}
			options.Outbounds = append(options.Outbounds, opts)
			existingTags[tag] = struct{}{}
		case option.Endpoint:
			tag := opts.Tag
			if tag == "" {
				continue
			}
			if _, exists := existingTags[tag]; exists {
				continue
			}
			options.Endpoints = append(options.Endpoints, opts)
			existingTags[tag] = struct{}{}
		}
	}
}

// optionTagSet returns the outbound and endpoint tags already present in the
// box options.
func optionTagSet(options option.Options) map[string]struct{} {
	tags := make(map[string]struct{}, len(options.Outbounds)+len(options.Endpoints))
	for _, out := range options.Outbounds {
		tags[out.Tag] = struct{}{}
	}
	for _, ep := range options.Endpoints {
		tags[ep.Tag] = struct{}{}
	}
	return tags
}

func (r *LocalBackend) DisconnectVPN() error {
	return r.vpnClient.Disconnect()
}

func (r *LocalBackend) RestartVPN() error {
	bOptions := r.getBoxOptions()
	return r.vpnClient.Restart(bOptions)
}

// SelectServer selects the server identified by tag. The empty string is treated as [vpn.AutoSelectTag].
func (r *LocalBackend) SelectServer(tag string) error {
	if tag == "" {
		tag = vpn.AutoSelectTag
	}
	if err := r.vpnClient.SelectServer(tag); err != nil {
		return fmt.Errorf("failed to select server: %w", err)
	}
	r.persistSelection(tag)
	if r.vpnClient.Status() == vpn.Connected {
		if sel, _, err := r.SelectedServer(); err == nil && sel != nil {
			r.sessionHistory.HandleServerChange(sel.Tag, sel.Location.City, sel.Location.Country)
		}
	}
	return nil
}

// persistSelection records the user's server choice in settings. tag must be
// AutoSelectTag or the tag of a server known to the manager.
func (r *LocalBackend) persistSelection(tag string) {
	if tag == vpn.AutoSelectTag {
		if err := settings.Patch(settings.Settings{
			settings.AutoConnectKey:    true,
			settings.SelectedServerKey: nil,
		}); err != nil {
			slog.Warn("failed to update settings", "error", err)
		}
		return
	}
	server, found := r.srvManager.GetServerByTag(tag)
	if !found {
		slog.Warn("no server found for tag, skipping settings persistence", "tag", tag)
		return
	}
	server.Options = nil
	if err := settings.Patch(settings.Settings{
		settings.AutoConnectKey:    false,
		settings.SelectedServerKey: server,
	}); err != nil {
		slog.Warn("Failed to save selected server in settings", "error", err)
		return
	}
	slog.Info("Selected server", "tag", tag, "type", server.Type)
}

// VPNConnections returns a list of the active connections. If there are no connections and the
// tunnel is open, an empty slice is returned without an error.
func (r *LocalBackend) VPNConnections() ([]vpn.Connection, error) {
	return r.vpnClient.Connections()
}

func (r *LocalBackend) VPNThroughput() (vpn.ThroughputSnapshot, error) {
	return r.vpnClient.Throughput()
}

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
		tag, err := r.vpnClient.CurrentSelectedServer()
		if err != nil {
			return nil, false, fmt.Errorf("failed to get current selected server: %w", err)
		}
		if tag == "" {
			return nil, false, fmt.Errorf("no selected server")
		}
		server, found := r.srvManager.GetServerByTag(tag)
		return server, found, nil
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

func (r *LocalBackend) startSessionAutoSelectListener() {
	events.SubscribeContext(r.ctx, func(evt vpn.AutoSelectedEvent) {
		if evt.Selected == "" || r.vpnClient.Status() != vpn.Connected {
			return
		}
		var city, country string
		if server, found := r.srvManager.GetServerByTag(evt.Selected); found {
			city = server.Location.City
			country = server.Location.Country
		}
		r.sessionHistory.HandleServerChange(evt.Selected, city, country)
	})
}

func (r *LocalBackend) startAutoSelectedListener() {
	var (
		mu     sync.Mutex
		cancel context.CancelFunc
		done   <-chan struct{}
	)
	events.SubscribeContext(r.ctx, func(evt vpn.StatusUpdateEvent) {
		mu.Lock()
		defer mu.Unlock()
		if cancel != nil {
			cancel()
			<-done
			cancel = nil
		}
		if evt.Status == vpn.Connected {
			var ctx context.Context
			ctx, cancel = context.WithCancel(r.ctx)
			done = r.vpnClient.AutoSelectedChangeListener(ctx)
		}
	})
}

// ClearTunnelCache removes the tunnel cache from the configured data directory.
// If removal was deferred because the tunnel was active, it restarts the tunnel
// to apply the clear immediately.
func (r *LocalBackend) ClearTunnelCache() error {
	shouldRestart, err := r.vpnClient.ClearTunnelCache(settings.GetString(settings.DataPathKey))
	if err != nil || !shouldRestart {
		return err
	}

	err = r.RestartVPN()
	if err == nil || errors.Is(err, vpn.ErrTunnelNotConnected) {
		return nil
	}
	return err
}

func (r *LocalBackend) RunOfflineURLTests() error {
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		return fmt.Errorf("no config available: %w", err)
	}
	svrs := r.srvManager.AllServers()
	slog.Debug("Running offline URL tests", "server_count", len(svrs), "url_override_count", len(cfg.BanditURLOverrides))
	results, err := r.vpnClient.RunOfflineURLTests(
		r.ctx,
		settings.GetString(settings.DataPathKey),
		servers.ServerList{Servers: svrs}.Outbounds(),
		cfg.BanditURLOverrides,
	)
	if err != nil {
		return err
	}
	now := time.Now()
	histories := make(map[string]servers.SelectionHistory, len(results))
	for tag, delay := range results {
		// Offline pre-warm produces only a single success delay per tag;
		// shape it as a probe-success snapshot so the production tunnel's
		// AutoSelectHistoryStorage can seed cold-start ranking from it
		// without further translation.
		histories[tag] = lbA.TagHistory{
			LastSuccessDelayMs: uint32(delay),
			LastOutcomeAt:      now,
			UpdatedAt:          now,
		}
	}
	if len(histories) > 0 {
		if err := r.srvManager.UpdateSelectionHistory(histories); err != nil {
			slog.Warn("Failed to persist offline selection history", "error", err)
		}
		events.Emit(vpn.URLTestCompleteEvent{Source: vpn.URLTestSourceOffline, Count: len(histories), Results: results})
		selected, err := r.vpnClient.CurrentAutoSelectedServer()
		if err != nil {
			slog.Warn("Failed to get current auto-selected server after URL tests", "error", err)
		} else {
			events.Emit(vpn.AutoSelectedEvent{Selected: selected})
		}
	}
	return nil
}

// defaultExhaustionRefetchGap rate-limits exhaustion-driven refetches
// so a misbehaving config handler can't hammer /config-new.
var defaultExhaustionRefetchGap = time.Minute

// exhaustionGate rate-limits exhaustion-driven refetches. lastAt is
// recorded before the fetch runs so a failing fetcher doesn't
// tight-loop.
type exhaustionGate struct {
	mu     sync.Mutex
	lastAt time.Time
}

func (g *exhaustionGate) allow() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if !g.lastAt.IsZero() && now.Sub(g.lastAt) < defaultExhaustionRefetchGap {
		return false
	}
	g.lastAt = now
	return true
}

func (r *LocalBackend) refetchOnExhaustion() {
	if !r.exhaustionGate.allow() {
		return
	}
	if err := r.confHandler.Fetch(); err != nil {
		slog.Warn("MutableAutoSelect exhaustion refetch failed", "error", err)
	}
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

func (r *LocalBackend) VerifyPassword(ctx context.Context, email, password string) error {
	_, err := r.accountClient.VerifyPassword(ctx, email, password)
	return err
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

func (r *LocalBackend) OAuthDeviceLimitCallback(ctx context.Context, oAuthToken string) error {
	return r.accountClient.OAuthDeviceLimitCallback(ctx, oAuthToken)
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

func (r *LocalBackend) NewStripeSubscription(ctx context.Context, email, planID, couponCode string) (string, error) {
	return r.accountClient.NewStripeSubscription(ctx, email, planID, couponCode)
}

func (r *LocalBackend) PaymentRedirect(ctx context.Context, data account.PaymentRedirectData) (string, error) {
	return r.accountClient.PaymentRedirect(ctx, data)
}

func (r *LocalBackend) ReferralAttach(ctx context.Context, code, channel string) (*account.ReferralAttachResponse, error) {
	return r.accountClient.ReferralAttach(ctx, code, channel)
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

func (r *LocalBackend) RestoreSubscription(ctx context.Context, service account.SubscriptionService, data map[string]string) (*account.RestoreSubscriptionResponse, error) {
	return r.accountClient.RestoreSubscription(ctx, service, data)
}
