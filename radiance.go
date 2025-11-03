// Package radiance provides a local server that proxies all requests to a remote proxy server using different
// protocols meant to circumvent censorship. Radiance uses a [transport.StreamDialer] to dial the target server
// over the desired protocol. The [config.Config] is used to configure the dialer for a proxy server.
package radiance

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/Xuanwo/go-locale"
	"github.com/getlantern/kindling"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	lcommon "github.com/getlantern/common"
	"github.com/getlantern/radiance/api"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/deviceid"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/fronted"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/telemetry"
	"github.com/getlantern/radiance/traces"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/issue"
)

const configPollInterval = 10 * time.Minute
const tracerName = "github.com/getlantern/radiance"

//go:generate mockgen -destination=radiance_mock_test.go -package=radiance github.com/getlantern/radiance configHandler

// configHandler is an interface that abstracts the config.ConfigHandler struct for easier testing.
type configHandler interface {
	// Stop stops the config handler from fetching new configurations.
	Stop()
	// SetPreferredServerLocation sets the preferred server location. If not set - it's auto selected by the API
	SetPreferredServerLocation(country, city string)
	// GetConfig returns the current configuration.
	// It returns an error if the configuration is not yet available.
	GetConfig() (*config.Config, error)

	// AddConfigListener adds a listener that is called whenever the configuration changes.
	AddConfigListener(listener config.ListenerFunc)
}

type issueReporter interface {
	Report(ctx context.Context, report issue.IssueReport, userEmail, country string) error
}

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
type Radiance struct {
	confHandler   configHandler
	issueReporter issueReporter
	apiHandler    *api.APIClient
	srvManager    *servers.Manager

	// user config is the user config object that contains the device ID and other user data
	userInfo common.UserInfo
	locale   string

	shutdownFuncs []func(context.Context) error
	closeOnce     sync.Once
	stopChan      chan struct{}
	shutdownOTEL  func(context.Context) error
}

type Options struct {
	DataDir  string
	LogDir   string
	Locale   string
	DeviceID string
	LogLevel string
}

// NewRadiance creates a new Radiance VPN client. opts includes the platform interface used to
// interact with the underlying platform on iOS, Android, and MacOS. On other platforms, it is
// ignored and can be nil.
func NewRadiance(opts Options) (*Radiance, error) {
	if opts.Locale == "" {
		// It is preferable to use the locale from the frontend, as locale is a requirement for lots
		// of frontend code and therefore is more reliably supported there.
		// However, if the frontend locale is not available, we can use the system locale as a fallback.
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

	shutdownFuncs := []func(context.Context) error{}
	if err := common.Init(opts.DataDir, opts.LogDir, opts.LogLevel); err != nil {
		return nil, fmt.Errorf("failed to initialize: %w", err)
	}

	dataDir := common.DataPath()
	f, err := fronted.NewFronted(reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"), &slogWriter{Logger: slog.Default()})
	if err != nil {
		return nil, fmt.Errorf("failed to create fronted: %w", err)
	}

	k := kindling.NewKindling(
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(&slogWriter{Logger: slog.Default()}),
		kindling.WithDomainFronting(f),
		kindling.WithProxyless("df.iantem.io"),
	)

	httpClientWithTimeout := k.NewHTTPClient()
	httpClientWithTimeout.Timeout = common.DefaultHTTPTimeout

	userInfo := common.NewUserConfig(platformDeviceID, dataDir, opts.Locale)
	apiHandler := api.NewAPIClient(httpClientWithTimeout, userInfo, dataDir)
	issueReporter, err := issue.NewIssueReporter(httpClientWithTimeout, userInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to create issue reporter: %w", err)
	}
	svrMgr, err := servers.NewManager(dataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create server manager: %w", err)
	}
	cOpts := config.Options{
		PollInterval: configPollInterval,
		HTTPClient:   httpClientWithTimeout,
		SvrManager:   svrMgr,
		User:         userInfo,
		DataDir:      dataDir,
		Locale:       opts.Locale,
		APIHandler:   apiHandler,
	}
	if disableFetch, ok := env.Get[bool](env.DisableFetch); ok && disableFetch {
		cOpts.PollInterval = -1
		slog.Info("Disabling config fetch")
	}
	confHandler := config.NewConfigHandler(cOpts)
	confHandler.AddConfigListener(
		func(oldConfig, newConfig *config.Config) error {
			slog.Info("Config changed", "oldConfig", oldConfig, "newConfig", newConfig)
			return telemetry.OnNewConfig(oldConfig, newConfig, platformDeviceID, userInfo)
		},
	)
	r := &Radiance{
		confHandler:   confHandler,
		issueReporter: issueReporter,
		apiHandler:    apiHandler,
		srvManager:    svrMgr,
		userInfo:      userInfo,
		locale:        opts.Locale,
		shutdownFuncs: shutdownFuncs,
		stopChan:      make(chan struct{}),
		closeOnce:     sync.Once{},
	}
	r.addShutdownFunc(common.Close, telemetry.Close)
	return r, nil
}

// addShutdownFunc adds a shutdown function(s) to the Radiance instance.
// This function is called when the Radiance instance is closed to ensure that all
// resources are cleaned up properly.
func (r *Radiance) addShutdownFunc(fns ...func(context.Context) error) {
	for _, fn := range fns {
		if fn != nil {
			r.shutdownFuncs = append(r.shutdownFuncs, fn)
		}
	}
}

func (r *Radiance) Close() {
	r.closeOnce.Do(func() {
		slog.Debug("Closing Radiance")
		r.confHandler.Stop()
		close(r.stopChan)
		for _, shutdown := range r.shutdownFuncs {
			if err := shutdown(context.Background()); err != nil {
				slog.Error("Failed to shutdown", "error", err)
			}
		}
	})
	<-r.stopChan
}

func (r *Radiance) AddConfigListener(onChange func()) {
	r.confHandler.AddConfigListener(func(oldCfg, newCfg *config.Config) error {
		slog.Debug("Config Listener called")
		onChange()
		return nil
	})
}

// APIHandler returns the API handler for the Radiance client.
func (r *Radiance) APIHandler() *api.APIClient {
	return r.apiHandler
}

// SetPreferredServer sets the preferred server location for the VPN connection.
// pass empty strings to auto select the server location
func (r *Radiance) SetPreferredServer(ctx context.Context, country, city string) {
	r.confHandler.SetPreferredServerLocation(country, city)
}

// UserInfo returns the user info object for this client
// This is the user config object that contains the device ID and other user data
func (r *Radiance) UserInfo() common.UserInfo {
	return r.userInfo
}

// ServerManager returns the server manager for the Radiance client.
func (r *Radiance) ServerManager() *servers.Manager {
	return r.srvManager
}

type IssueReport = issue.IssueReport

// ReportIssue submits an issue report to the back-end with an optional user email
func (r *Radiance) ReportIssue(email string, report IssueReport) error {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "report_issue")
	defer span.End()
	if report.Type == "" && report.Description == "" {
		return fmt.Errorf("issue report should contain at least type or description")
	}
	var country string
	// get country from the config returned by the backend
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		slog.Warn("Failed to get config", "error", err)
	} else {
		country = cfg.ConfigResponse.Country
	}

	err = r.issueReporter.Report(ctx, report, email, country)
	if err != nil {
		slog.Error("Failed to report issue", "error", err)
		return traces.RecordError(ctx, fmt.Errorf("failed to report issue: %w", err))
	}
	slog.Info("Issue reported successfully")
	return nil
}

// Features returns the features available in the current configuration, returned from the server in the
// config response.
func (r *Radiance) Features() map[string]bool {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "features")
	defer span.End()
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		slog.Error("Failed to get config for features", "error", err)
		traces.RecordError(ctx, err, trace.WithStackTrace(true))
		return map[string]bool{}
	}
	if cfg == nil {
		slog.Info("No config available for features, returning empty map")
		return map[string]bool{}
	}
	slog.Debug("Returning features from config", "features", cfg.ConfigResponse.Features)
	// Return the features from the config
	if cfg.ConfigResponse.Features == nil {
		slog.Info("No features available in config, returning empty map")
		return map[string]bool{}
	}
	return cfg.ConfigResponse.Features
}

// ServerLocations returns the list of server locations where the user can connect to proxies.
func (r *Radiance) ServerLocations() ([]lcommon.ServerLocation, error) {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "server_locations")
	defer span.End()
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		slog.Error("Failed to get config for server locations", "error", err)
		traces.RecordError(ctx, err, trace.WithStackTrace(true))
		return nil, fmt.Errorf("failed to get config: %w", err)
	}
	if cfg == nil {
		slog.Info("No config available for server locations, returning error")
		traces.RecordError(ctx, err, trace.WithStackTrace(true))
		return nil, fmt.Errorf("no config available")
	}
	slog.Debug("Returning server locations from config", "locations", cfg.ConfigResponse.Servers)
	return cfg.ConfigResponse.Servers, nil
}

type slogWriter struct {
	*slog.Logger
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	// Convert the byte slice to a string and log it
	w.Info(string(p))
	return len(p), nil
}
