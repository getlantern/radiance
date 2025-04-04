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
	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/fronted"
	"github.com/getlantern/kindling"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/client"
	boxservice "github.com/getlantern/radiance/client/service"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/issue"
	"github.com/getlantern/radiance/metrics"
	"github.com/getlantern/radiance/user"
)

var log *slog.Logger

const configPollInterval = 10 * time.Minute

//go:generate mockgen -destination=radiance_mock_test.go -package=radiance github.com/getlantern/radiance configHandler
//go:generate mockgen -destination=vpn_client_test.go -package=radiance github.com/getlantern/radiance/client VPNClient

// configHandler is an interface that abstracts the config.ConfigHandler struct for easier testing.
type configHandler interface {
	// GetConfig returns the current proxy configuration and the country.
	GetConfig(ctx context.Context) ([]*config.Config, string, error)
	// Stop stops the config handler from fetching new configurations.
	Stop()

	// SetPreferredServerLocation sets the preferred server location. If not set - it's auto selected by the API
	SetPreferredServerLocation(country, city string)

	// ListAvailableServers lists the available server locations to choose from.
	ListAvailableServers(ctx context.Context) ([]config.AvailableServerLocation, error)
}

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
type Radiance struct {
	client.VPNClient

	confHandler  configHandler
	activeConfig *atomic.Value
	stopChan     chan struct{}

	user *user.User

	issueReporter *issue.IssueReporter
	logsDir       string
	shutdownFuncs []func(context.Context) error
	closeOnce     sync.Once

	configuredServersMutex sync.Locker
	configuredServers      map[string]boxservice.ServerConnectConfig
}

// NewRadiance creates a new Radiance VPN client. platIfce is the platform interface used to
// interact with the underlying platform on iOS and Android. On other platforms, it is ignored and
// can be nil.
func NewRadiance(opts client.Options) (*Radiance, error) {
	reporting.Init()

	dataDirPath, logDir, err := setupDirs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to setup directories: %w", err)
	}

	logPath := filepath.Join(logDir, app.LogFileName)
	var logWriter io.Writer
	log, logWriter, err = newLog(logPath)
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

<<<<<<< HEAD
	vpnC, err := client.NewVPNClient(dataDirPath, logDir, platIfce)
=======
	opts.DataDir = dataDirPath
	vpnC, err := client.NewVPNClient(opts, logDir)
>>>>>>> 4562ec67ed035c6e8316d1cdf36aa26ba819da97
	if err != nil {
		log.Error("Failed to create VPN client", "error", err)
		return nil, fmt.Errorf("failed to create VPN client: %w", err)
	}

	// TODO: Ideally we would know the user locale to set the country on fronted startup.
	f, err := newFronted(logWriter, reporting.PanicListener, filepath.Join(dataDirPath, "fronted_cache.json"))
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

	return &Radiance{
		VPNClient:     vpnC,
		confHandler:   config.NewConfigHandler(configPollInterval, k.NewHTTPClient(), u, dataDirPath),
		activeConfig:  new(atomic.Value),
		stopChan:      make(chan struct{}),
		user:          u,
		issueReporter: issueReporter,
		// TODO: after we start to persist data, we should update this implementation
		// for loading the configured servers and also the custom servers
		configuredServers:      make(map[string]boxservice.ServerConnectConfig),
		configuredServersMutex: new(sync.Mutex),
		logsDir:       logDir,
		shutdownFuncs: shutdownFuncs,
	}, nil
}

// TODO: the server stuff should probably be moved to the VPNClient as well..

func (r *Radiance) GetAvailableServers(ctx context.Context) ([]config.AvailableServerLocation, error) {
	return r.confHandler.ListAvailableServers(ctx)
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

// ServerLocation is the location of a remote VPN server.
type ServerLocation config.ProxyConnectConfig_ProxyLocation

// Server represents a remote VPN server.
type Server struct {
	Address  string
	Location ServerLocation
	Protocol string
}

// GetActiveServer returns the remote VPN server this client is currently connected to.
// It returns nil when VPN is disconnected
func (r *Radiance) GetActiveServer() (*Server, error) {
	if !r.ConnectionStatus() {
		return nil, nil
	}
	activeConfig := r.activeConfig.Load()
	if activeConfig == nil {
		return nil, fmt.Errorf("no active server config")
	}
	config := activeConfig.(*config.Config)

	return &Server{
		Address:  config.GetAddr(),
		Location: ServerLocation(*config.GetLocation()),
		Protocol: config.GetProtocol(),
	}, nil
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
	// get country from the config returned by the backend
	_, country, err := r.confHandler.GetConfig(eventual.DontWait)
	if err != nil {
		log.Error("Failed to get country", "error", err)
		country = ""
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

func setupDirs(baseDir string) (dataDir, logDir string, err error) {
	// On Windows, Mac, and Linux, we can easily determine the user directories in Go. Typically mobile will have
	// to pass the base directory to use.
	if baseDir == "" {
		return appdir.General(app.Name), appdir.Logs(app.Name), nil
	}
	logDir = filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", "", fmt.Errorf("failed to setup data directory: %w", err)
	}
	return baseDir, logDir, nil
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

<<<<<<< HEAD
func (r *Radiance) AddCustomServer(tag string, cfg boxservice.ServerConnectConfig) error {
	return r.vpnClient.AddCustomServer(tag, cfg)
}

func (r *Radiance) SelectCustomServer(tag string) error {
	return r.vpnClient.SelectCustomServer(tag)
}

func (r *Radiance) RemoveCustomServer(tag string) error {
	return r.vpnClient.RemoveCustomServer(tag)
=======
// SplitTunnelHandler returns the split tunnel handler for the VPN client.
func (r *Radiance) SplitTunnelHandler() *client.SplitTunnel {
	return r.VPNClient.SplitTunnelHandler()
>>>>>>> 4562ec67ed035c6e8316d1cdf36aa26ba819da97
}
