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
	"sync"
	"time"

	"github.com/Xuanwo/go-locale"
	C "github.com/getlantern/common"
	"github.com/getlantern/fronted"
	"github.com/getlantern/kindling"

	"github.com/getlantern/radiance/api"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/deviceid"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/reporting"

	boxservice "github.com/getlantern/radiance/client/service"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/issue"

	"github.com/getlantern/radiance/metrics"
)

const configPollInterval = 10 * time.Minute

//go:generate mockgen -destination=radiance_mock_test.go -package=radiance github.com/getlantern/radiance configHandler

// configHandler is an interface that abstracts the config.ConfigHandler struct for easier testing.
type configHandler interface {
	// Stop stops the config handler from fetching new configurations.
	Stop()

	// SetPreferredServerLocation sets the preferred server location. If not set - it's auto selected by the API
	SetPreferredServerLocation(country, city string)

	// ListAvailableServers returns a list of available server locations.
	ListAvailableServers() ([]C.ServerLocation, error)

	// GetConfig returns the current configuration.
	// It returns an error if the configuration is not yet available.
	GetConfig() (*config.Config, error)
}

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
type Radiance struct {
	confHandler   configHandler
	issueReporter *issue.IssueReporter
	apiHandler    *api.APIClient

	//user config is the user config object that contains the device ID and other user data
	userInfo common.UserInfo
	locale   string

	shutdownFuncs []func(context.Context) error
	closeOnce     sync.Once
	stopChan      chan struct{}
}

type Options struct {
	DataDir  string
	LogDir   string
	Locale   string
	DeviceID string
	// log level. This can be overridden by the RADIANCE_LOG_LEVEL env variable.
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
	if common.IsAndroid() || common.IsIOS() {
		platformDeviceID = opts.DeviceID
	} else {
		platformDeviceID = deviceid.Get()
	}

	dataDir := common.DataPath()
	f, err := newFronted(reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to create fronted: %w", err)
	}

	k := kindling.NewKindling(
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(&slogWriter{Logger: slog.Default().With("service", "kindling")}),
		kindling.WithDomainFronting(f),
		kindling.WithProxyless("api.iantem.io"),
	)

	shutdownFuncs := []func(context.Context) error{}
	shutdownMetrics, err := metrics.SetupOTelSDK(context.Background())
	if err != nil {
		slog.Error("Failed to setup OpenTelemetry SDK", "error", err)
	} else if shutdownMetrics != nil {
		shutdownFuncs = append(shutdownFuncs, shutdownMetrics)
		slog.Debug("Setup OpenTelemetry SDK", "shutdown", shutdownMetrics)
	}

	userInfo := common.NewUserConfig(platformDeviceID, dataDir, opts.Locale)
	apiHandler := api.NewAPIClient(k.NewHTTPClient(), userInfo, dataDir)
	issueReporter, err := issue.NewIssueReporter(k.NewHTTPClient(), apiHandler, userInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to create issue reporter: %w", err)
	}
	cOpts := config.Options{
		PollInterval:     configPollInterval,
		HTTPClient:       k.NewHTTPClient(),
		User:             userInfo,
		DataDir:          dataDir,
		ConfigRespParser: boxservice.UnmarshalConfig,
		Locale:           opts.Locale,
	}
	if disableFetch, ok := env.Get[bool](env.DisableFetch); ok && disableFetch {
		cOpts.PollInterval = -1
		slog.Info("Disabling config fetch")
	}
	confHandler := config.NewConfigHandler(cOpts)
	return &Radiance{
		confHandler:   confHandler,
		issueReporter: issueReporter,
		apiHandler:    apiHandler,
		userInfo:      userInfo,
		locale:        opts.Locale,
		shutdownFuncs: shutdownFuncs,
		stopChan:      make(chan struct{}),
		closeOnce:     sync.Once{},
	}, nil
}

// APIHandler returns the API handler for the Radiance client.
func (r *Radiance) APIHandler() *api.APIClient {
	return r.apiHandler
}

// TODO: The server-related functionality should probably be moved to the VPNClient as well.
// Eventually, this functionality will be moved to the API handler for better separation of concerns.
func (r *Radiance) GetAvailableServers() ([]C.ServerLocation, error) {
	return r.confHandler.ListAvailableServers()
}

// SetPreferredServer sets the preferred server location for the VPN connection.
// pass empty strings to auto select the server location
func (r *Radiance) SetPreferredServer(ctx context.Context, country, city string) {
	r.confHandler.SetPreferredServerLocation(country, city)
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

// UserInfo returns the user info object for this client
// This is the user config object that contains the device ID and other user data
func (r *Radiance) UserInfo() common.UserInfo {
	return r.userInfo
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
	if report.Type == "" && report.Description == "" {
		return fmt.Errorf("issue report should contain at least type or description")
	}
	// get issue type as integer
	typeInt, ok := issueTypeMap[report.Type]
	if !ok {
		slog.Error("Unknown issue type: %s, set to Other", "type", report.Type)
		typeInt = 9
	}
	var country string
	// get country from the config returned by the backend
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		slog.Error("Failed to get country", "error", err)
		country = ""
	} else {
		country = cfg.ConfigResponse.Country
	}

	return r.issueReporter.Report(
		common.LogPath(),
		email,
		typeInt,
		report.Description,
		report.Attachment,
		report.Device,
		report.Model,
		country,
	)
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

	logWriter := &slogWriter{Logger: slog.Default().With("service", "kindling", "group", "smartdialer")}

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
	fronted.SetLogger(slog.Default().With("module", "fronted"))
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
	lz.smartTransportMu.Lock()

	if lz.smartTransport == nil {
		slog.Info("Smart transport is nil")
		var err error
		lz.smartTransport, err = kindling.NewSmartHTTPTransport(lz.logWriter, lz.domain)
		if err != nil {
			slog.Info("Error creating smart transport", "error", err)
			lz.smartTransportMu.Unlock()
			// This typically just means we're offline
			return nil, fmt.Errorf("could not create smart transport -- offline? %v", err)
		}
	}

	lz.smartTransportMu.Unlock()
	return lz.smartTransport.RoundTrip(req)
}

type slogWriter struct {
	*slog.Logger
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	// Convert the byte slice to a string and log it
	w.Info(string(p))
	return len(p), nil
}
