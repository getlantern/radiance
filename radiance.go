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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/appdir"
	C "github.com/getlantern/common"
	"github.com/getlantern/fronted"
	"github.com/getlantern/kindling"

	"github.com/Xuanwo/go-locale"
	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/client"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/issue"
	"github.com/getlantern/radiance/metrics"
	"github.com/getlantern/radiance/user"
)

var log *slog.Logger

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
	client.VPNClient

	confHandler  configHandler
	activeServer *atomic.Value
	stopChan     chan struct{}

	user *user.User

	issueReporter *issue.IssueReporter
	logsDir       string
	shutdownFuncs []func(context.Context) error
	closeOnce     sync.Once
}

// NewRadiance creates a new Radiance VPN client. platIfce is the platform interface used to
// interact with the underlying platform on iOS and Android. On other platforms, it is ignored and
// can be nil.
func NewRadiance(opts client.Options) (*Radiance, error) {
	reporting.Init()
	if opts.DataDir == "" {
		opts.DataDir = appdir.General(app.Name)
	}
	if opts.LogDir == "" {
		opts.LogDir = appdir.Logs(app.Name)
	}
	if opts.Locale == "" {
		// It is preferable to use the locale from the frontend, as locale is a requirement for lots
		// of frontend code and therefore is more reliably supported there.
		// However, if the frontend locale is not available, we can use the system locale as a fallback.
		if tag, err := locale.Detect(); err != nil {
			log.Info("Failed to detect locale", "error", err)
			opts.Locale = "en-US"
		} else {
			opts.Locale = tag.String()
		}
	}
	mkdirs(&opts)

	var logWriter io.Writer
	var err error
	log, logWriter, err = newLog(filepath.Join(opts.LogDir, app.LogFileName))
	if err != nil {
		return nil, fmt.Errorf("could not create log: %w", err)
	}
	shutdownFuncs := []func(context.Context) error{}
	shutdownMetrics, err := metrics.SetupOTelSDK(context.Background())
	if err != nil {
		log.Error("Failed to setup OpenTelemetry SDK", "error", err)
	} else if shutdownMetrics != nil {
		shutdownFuncs = append(shutdownFuncs, shutdownMetrics)
		log.Debug("Setup OpenTelemetry SDK", "shutdown", shutdownMetrics)
	}

	vpnC, err := client.NewVPNClient(opts)
	if err != nil {
		log.Error("Failed to create VPN client", "error", err)
		return nil, fmt.Errorf("failed to create VPN client: %w", err)
	}

	// TODO: Ideally we would know the user locale to set the country on fronted startup.
	f, err := newFronted(logWriter, reporting.PanicListener, filepath.Join(opts.DataDir, "fronted_cache.json"))
	if err != nil {
		log.Error("Failed to create fronted", "error", err)
		return nil, fmt.Errorf("failed to create fronted: %w", err)
	}
	k := kindling.NewKindling(
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(logWriter),
		kindling.WithDomainFronting(f),
		kindling.WithProxyless("api.iantem.io"),
	)
	u := user.New(k.NewHTTPClient())
	issueReporter, err := issue.NewIssueReporter(k.NewHTTPClient(), u)
	if err != nil {
		return nil, fmt.Errorf("failed to create issue reporter: %w", err)
	}
	confHandler := config.NewConfigHandler(configPollInterval, k.NewHTTPClient(), u, opts.DataDir, vpnC.ParseConfig, opts.Locale)
	confHandler.AddConfigListener(vpnC.OnNewConfig)

	return &Radiance{
		VPNClient:     vpnC,
		confHandler:   confHandler,
		activeServer:  new(atomic.Value),
		stopChan:      make(chan struct{}),
		user:          u,
		issueReporter: issueReporter,
		logsDir:       opts.LogDir,
		shutdownFuncs: shutdownFuncs,
	}, nil
}

// TODO: the server stuff should probably be moved to the VPNClient as well..

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
		log.Debug("Closing Radiance")
		r.confHandler.Stop()
		close(r.stopChan)
		for _, shutdown := range r.shutdownFuncs {
			if err := shutdown(context.Background()); err != nil {
				log.Error("Failed to shutdown", "error", err)
			}
		}
	})
}

// Server represents a remote VPN server.
type Server struct {
	Address  string
	Location C.ServerLocation
	Protocol string
}

// GetActiveServer returns the remote VPN server this client is currently connected to.
// It returns nil when VPN is disconnected
func (r *Radiance) GetActiveServer() (*Server, error) {
	if !r.ConnectionStatus() {
		return nil, fmt.Errorf("VPN is not connected")
	}
	activeServer := r.activeServer.Load()
	if activeServer == nil {
		return nil, fmt.Errorf("no active server config")
	}

	return activeServer.(*Server), nil

}

// User returns the user object for this client
func (r *Radiance) User() *user.User {
	return r.user
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
		log.Error("Unknown issue type: %s, set to Other", "type", report.Type)
		typeInt = 9
	}
	var country string
	// get country from the config returned by the backend
	cfg, err := r.confHandler.GetConfig()
	if err != nil {
		log.Error("Failed to get country", "error", err)
		country = ""
	} else {
		country = cfg.ConfigResponse.Country
	}

	return r.issueReporter.Report(
		r.logsDir,
		email,
		typeInt,
		report.Description,
		report.Attachment,
		report.Device,
		report.Model,
		country)
}

func mkdirs(opts *client.Options) {
	// Make sure the data and logs dirs exist
	for _, dir := range []string{opts.DataDir, opts.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Error("Failed to create data directory", "dir", dir, "error", err)
		}
	}
}

// Return an slog logger configured to write to both stdout and the log file.
func newLog(logPath string) (*slog.Logger, io.Writer, error) {
	// If the log file does not exist, create it.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open log file: %w", err)
	}
	// defer f.Close() - file should be closed externally when logger is no longer needed
	logWriter := io.MultiWriter(os.Stdout, f)
	logger := slog.New(slog.NewTextHandler(logWriter, nil))
	slog.SetDefault(logger)
	return logger, logWriter, nil
}

func newFronted(logWriter io.Writer, panicListener func(string), cacheFile string) (fronted.Fronted, error) {
	// Parse the domain from the URL.
	configURL := "https://raw.githubusercontent.com/getlantern/lantern-binaries/refs/heads/main/fronted.yaml.gz"
	u, err := url.Parse(configURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %v", err)
	}
	// Extract the domain from the URL.
	domain := u.Host

	// First, download the file from the specified URL using the smart dialer.
	// Then, create a new fronted instance with the downloaded file.
	trans, err := kindling.NewSmartHTTPTransport(logWriter, domain)
	if err != nil {
		return nil, fmt.Errorf("failed to create smart HTTP transport: %v", err)
	}
	httpClient := &http.Client{
		Transport: trans,
	}
	return fronted.NewFronted(
		fronted.WithPanicListener(panicListener),
		fronted.WithCacheFile(cacheFile),
		fronted.WithHTTPClient(httpClient),
		fronted.WithConfigURL(configURL),
	), nil
}

// SplitTunnelHandler returns the split tunnel handler for the VPN client.
func (r *Radiance) SplitTunnelHandler() *client.SplitTunnel {
	return r.VPNClient.SplitTunnelHandler()
}
