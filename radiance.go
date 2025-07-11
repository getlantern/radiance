// Package radiance provides a local server that proxies all requests to a remote proxy server using different
// protocols meant to circumvent censorship. Radiance uses a [transport.StreamDialer] to dial the target server
// over the desired protocol. The [config.Config] is used to configure the dialer for a proxy server.
package radiance

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/Xuanwo/go-locale"
	"github.com/getlantern/fronted"
	"github.com/getlantern/kindling"

	"github.com/getlantern/radiance/api"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/deviceid"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/servers"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/issue"

	"github.com/getlantern/radiance/metrics"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const configPollInterval = 10 * time.Minute

// Initially set the tracer to noop, so that we don't have to check for nil everywhere.
var tracer trace.Tracer = noop.Tracer{}

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
}

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
type Radiance struct {
	confHandler   configHandler
	issueReporter *issue.IssueReporter
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

// NewRadiance creates a new Radiance VPN client. platIfce is the platform interface used to
// interact with the underlying platform on iOS and Android. On other platforms, it is ignored and
// can be nil.
func NewRadiance(opts Options) (*Radiance, error) {
	if err := common.Init(opts.DataDir, opts.LogDir, opts.LogLevel); err != nil {
		return nil, fmt.Errorf("failed to initialize: %w", err)
	}
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

	dataDir := common.DataPath()
	f, err := newFronted(reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to create fronted: %w", err)
	}

	k := kindling.NewKindling(
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(&slogWriter{Logger: slog.Default()}),
		kindling.WithDomainFronting(f),
		kindling.WithProxyless("api.iantem.io"),
	)

	shutdownFuncs := []func(context.Context) error{}

	userInfo := common.NewUserConfig(platformDeviceID, dataDir, opts.Locale)
	apiHandler := api.NewAPIClient(k.NewHTTPClient(), userInfo, dataDir)
	issueReporter, err := issue.NewIssueReporter(k.NewHTTPClient(), apiHandler, userInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to create issue reporter: %w", err)
	}
	svrMgr, err := servers.NewManager(dataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create server manager: %w", err)
	}
	cOpts := config.Options{
		PollInterval: configPollInterval,
		HTTPClient:   k.NewHTTPClient(),
		SvrManager:   svrMgr,
		User:         userInfo,
		DataDir:      dataDir,
		Locale:       opts.Locale,
	}
	if disableFetch, ok := env.Get[bool](env.DisableFetch); ok && disableFetch {
		cOpts.PollInterval = -1
		slog.Info("Disabling config fetch from the API")
	}
	confHandler := config.NewConfigHandler(cOpts)

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
	confHandler.AddConfigListener(r.otelConfigListener)
	return r, nil
}

// otelConfigListener is a listener for OpenTelemetry configuration changes. Note this will be called both when
// new configurations are loaded from disk as well as over the network.
func (r *Radiance) otelConfigListener(oldConfig, newConfig *config.Config) error {
	if newConfig == nil {
		return fmt.Errorf("new config is nil")
	}
	// Check if the old OTEL configuration is the same as the new one.
	if oldConfig != nil && reflect.DeepEqual(oldConfig.ConfigResponse.OTEL, newConfig.ConfigResponse.OTEL) {
		slog.Debug("OpenTelemetry configuration has not changed, skipping initialization")
		return nil
	}

	slog.Info("OpenTelemetry configuration changed", "newConfig", newConfig.ConfigResponse.OTEL)
	if newConfig.ConfigResponse.OTEL.Endpoint == "" {
		slog.Info("OpenTelemetry endpoint is empty, not initializing OpenTelemetry SDK")
		return nil
	}
	if r.shutdownOTEL != nil {
		slog.Info("Shutting down existing OpenTelemetry SDK")
		if err := r.shutdownOTEL(context.Background()); err != nil {
			slog.Error("Failed to shutdown OpenTelemetry SDK", "error", err)
			return fmt.Errorf("failed to shutdown OpenTelemetry SDK: %w", err)
		}
		r.shutdownOTEL = nil
	}
	newConfig.ConfigResponse.OTEL.TracesSampleRate = 1.0 // Always sample traces for now

	err := r.startOTEL(context.Background(), newConfig)
	if err != nil {
		slog.Error("Failed to start OpenTelemetry SDK", "error", err)
		return fmt.Errorf("failed to start OpenTelemetry SDK: %w", err)
	}

	// Get a tracer for your application/package
	tracer = otel.Tracer("radiance")
	return nil
}

func (r *Radiance) startOTEL(ctx context.Context, cfg *config.Config) error {
	shutdown, err := metrics.SetupOTelSDK(ctx, cfg.ConfigResponse.OTEL)
	if shutdown != nil {
		r.shutdownOTEL = shutdown
		r.addShutdownFunc(shutdown)
	}
	// If the OpenTelemetry SDK could not be initialized, log the error and return.
	// This is not a fatal error, so we just log it and continue.
	if err != nil {
		return fmt.Errorf("failed to setup OpenTelemetry SDK: %w", err)
	}

	slog.Info("OpenTelemetry SDK initialized successfully", "endpoint", cfg.ConfigResponse.OTEL.Endpoint)
	return nil
}

// addShutdownFunc adds a shutdown function to the Radiance instance.
// This function is called when the Radiance instance is closed to ensure that all resources are cleaned up properly.
func (r *Radiance) addShutdownFunc(fn func(context.Context) error) {
	if fn != nil {
		r.shutdownFuncs = append(r.shutdownFuncs, fn)
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

// IssueReport represents a user report of a bug or service problem. This report can be submitted
// via [Radiance.ReportIssue].
type IssueReport struct {
	// Type is one of the predefined issue type strings
	Type string
	// Issue description
	Description string
	// Attachment is a list of issue attachments
	Attachment []*issue.Attachment

	// device common name
	Device string
	// device alphanumeric name
	Model string
}

// issue text to type mapping
var issueTypeMap = map[string]int{
	"Cannot complete purchase":    0,
	"Cannot sign in":              1,
	"Spinner loads endlessly":     2,
	"Cannot access blocked sites": 3,
	"Slow":                        4,
	"Cannot link device":          5,
	"Application crashes":         6,
	"Other":                       9,
	"Update fails":                10,
}

// ReportIssue submits an issue report to the back-end with an optional user email
func (r *Radiance) ReportIssue(email string, report *IssueReport) error {
	ctx, span := tracer.Start(context.Background(), "report-issue")
	defer span.End()
	if report.Type == "" && report.Description == "" {
		return fmt.Errorf("issue report should contain at least type or description")
	}
	// get issue type as integer
	typeInt, ok := issueTypeMap[report.Type]
	if !ok {
		slog.Error("Unknown issue type, setting to 'Other'", "type", report.Type)
		typeInt = 9
	}
	var country string
	// get country from the config returned by the backend
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		slog.Error("Failed to get country", "error", err)
		span.RecordError(err)
		country = ""
	} else {
		country = cfg.ConfigResponse.Country
	}

	err = r.issueReporter.Report(
		ctx,
		common.LogPath(),
		email,
		typeInt,
		report.Description,
		report.Attachment,
		report.Device,
		report.Model,
		country,
	)
	if err != nil {
		slog.Error("Failed to report issue", "error", err)
		span.RecordError(err)
		return fmt.Errorf("failed to report issue: %w", err)
	}
	slog.Info("Issue reported successfully")
	return nil
}

func newFronted(panicListener func(string), cacheFile string) (fronted.Fronted, error) {
	// Parse the domain from the URL.
	configURL := "https://raw.githubusercontent.com/getlantern/lantern-binaries/refs/heads/main/fronted.yaml.gz"
	u, err := url.Parse(configURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %v", err)
	}
	// Extract the domain from the URL.
	domain := u.Host

	logWriter := &slogWriter{Logger: slog.Default()}

	// First, download the file from the specified URL using the smart dialer.
	// Then, create a new fronted instance with the downloaded file.
	trans, err := kindling.NewSmartHTTPTransport(logWriter, domain)
	if err != nil {
		return nil, fmt.Errorf("failed to create smart HTTP transport: %v", err)
	}
	lz := &lazyDialingRoundTripper{
		smartTransportMu: sync.Mutex{},
		logWriter:        logWriter,
		domain:           domain,
	}
	if trans != nil {
		lz.smartTransport = trans
	}

	httpClient := &http.Client{
		Transport: lz,
	}
	fronted.SetLogger(slog.Default())
	return fronted.NewFronted(
		fronted.WithPanicListener(panicListener),
		fronted.WithCacheFile(cacheFile),
		fronted.WithHTTPClient(httpClient),
		fronted.WithConfigURL(configURL),
	), nil
}

// This is a lazy RoundTripper that allows radiance to start without an error
// when it's offline.
type lazyDialingRoundTripper struct {
	smartTransport   http.RoundTripper
	smartTransportMu sync.Mutex

	logWriter io.Writer
	domain    string
}

// Make sure lazyDialingRoundTripper implements http.RoundTripper
var _ http.RoundTripper = (*lazyDialingRoundTripper)(nil)

func (lz *lazyDialingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := tracer.Start(req.Context(), "lazy-dialing-roundtrip")
	defer span.End()
	lz.smartTransportMu.Lock()

	if lz.smartTransport == nil {
		slog.Info("Smart transport is nil")
		var err error
		lz.smartTransport, err = kindling.NewSmartHTTPTransport(lz.logWriter, lz.domain)
		if err != nil {
			slog.Info("Error creating smart transport", "error", err)
			lz.smartTransportMu.Unlock()
			span.RecordError(err)
			// This typically just means we're offline
			return nil, fmt.Errorf("could not create smart transport -- offline? %v", err)
		}
	}

	lz.smartTransportMu.Unlock()
	res, err := lz.smartTransport.RoundTrip(req.WithContext(ctx))
	if err != nil {
		span.RecordError(err)
	} else {
		span.SetAttributes(attribute.String("response.status", res.Status))
	}
	return res, err
}

type slogWriter struct {
	*slog.Logger
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	// Convert the byte slice to a string and log it
	w.Info(string(p))
	return len(p), nil
}
