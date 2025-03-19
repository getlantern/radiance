// Package radiance provides a local server that proxies all requests to a remote proxy server using different
// protocols meant to circumvent censorship. Radiance uses a [transport.StreamDialer] to dial the target server
// over the desired protocol. The [config.Config] is used to configure the dialer for a proxy server.
package radiance

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/sagernet/sing-box/experimental/libbox/platform"

	"github.com/getlantern/appdir"
	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/client"
	boxservice "github.com/getlantern/radiance/client/service"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/issue"
	"github.com/getlantern/radiance/transport"
	"github.com/getlantern/radiance/transport/proxyless"
	"github.com/getlantern/radiance/user"
)

var (
	vpnLogOutput = filepath.Join(logDir(), "lantern.log")
	log          = newLog(vpnLogOutput)

	configPollInterval = 10 * time.Minute
)

//go:generate mockgen -destination=radiance_mock_test.go -package=radiance github.com/getlantern/radiance httpServer,configHandler
//go:generate mockgen -destination=vpn_client_test.go -package=radiance github.com/getlantern/radiance/client VPNClient

// httpServer is an interface that abstracts the http.Server struct for easier testing.
type httpServer interface {
	Serve(listener net.Listener) error
	Shutdown(ctx context.Context) error
}

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
	vpnClient client.VPNClient

	srv          httpServer
	confHandler  configHandler
	activeConfig *atomic.Value

	connected   bool
	statusMutex sync.Locker
	stopChan    chan struct{}

	user *user.User

	configuredServersMutex sync.Locker
	configuredServers      map[string]boxservice.ServerConnectConfig
	issueReporter          *issue.IssueReporter
}

// NewRadiance creates a new Radiance server using an existing config.
func NewRadiance(platIfce platform.Interface) (*Radiance, error) {
	vpnC, err := client.NewVPNClient(vpnLogOutput, platIfce)
	if err != nil {
		return nil, err
	}

	// TODO: Ideally we would know the user locale here on radiance startup.
	k := kindling.NewKindling(
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithDomainFronting("https://raw.githubusercontent.com/getlantern/lantern-binaries/refs/heads/main/fronted.yaml.gz", ""),
		kindling.WithProxyless("api.iantem.io"),
	)
	user := user.New(k.NewHTTPClient())
	issueReporter, err := issue.NewIssueReporter(k.NewHTTPClient(), user)
	if err != nil {
		return nil, err
	}

	return &Radiance{
		vpnClient: vpnC,

		confHandler:   config.NewConfigHandler(configPollInterval, k.NewHTTPClient(), user),
		activeConfig:  new(atomic.Value),
		connected:     false,
		statusMutex:   new(sync.Mutex),
		stopChan:      make(chan struct{}),
		user:          user,
		issueReporter: issueReporter,
		// TODO: after we start to persist data, we should update this implementation
		// for loading the configured servers and also the custom servers
		configuredServers:      make(map[string]boxservice.ServerConnectConfig),
		configuredServersMutex: new(sync.Mutex),
	}, nil
}

func (r *Radiance) GetAvailableServers(ctx context.Context) ([]config.AvailableServerLocation, error) {
	return r.confHandler.ListAvailableServers(ctx)
}

// SetPreferredServer sets the preferred server location for the VPN connection.
// pass empty strings to auto select the server location
func (r *Radiance) SetPreferredServer(ctx context.Context, country, city string) {
	r.confHandler.SetPreferredServerLocation(country, city)
}

// Run starts the Radiance proxy server on the specified address.
// This function will be replaced by StartVPN as part of https://github.com/getlantern/engineering/issues/1883
func (r *Radiance) run(addr string) error {
	reporting.Init()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	log.Debug("Fetching config")
	configs, _, err := r.confHandler.GetConfig(ctx)
	cancel()
	if err != nil {
		r.setStatus(false)
		sentry.CaptureException(err)
		return fmt.Errorf("could not fetch config: %w", err)
	}

	var proxyConf, proxylessConf *config.Config
	for _, conf := range configs {
		if conf.GetConnectCfgProxyless() != nil {
			proxylessConf = conf
		}
		proxyConf = conf
		r.activeConfig.Store(conf)
	}

	dialer, err := transport.DialerFrom(proxyConf)
	if err != nil {
		r.setStatus(false)
		sentry.CaptureException(err)
		return fmt.Errorf("could not create dialer: %w", err)
	}
	log.Info("Creating dialer with config", "config", proxyConf)

	pAddr := fmt.Sprintf("%s:%d", proxyConf.Addr, proxyConf.Port)
	handler := proxyHandler{
		addr:      pAddr,
		authToken: proxyConf.AuthToken,
		dialer:    dialer,
		user:      r.user,
		client: http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.DialStream(ctx, pAddr)
				},
			},
		},
	}

	if proxylessConf != nil {
		handler.proxylessDialer, err = proxyless.NewStreamDialer(dialer, proxylessConf)
		if err != nil {
			sentry.CaptureException(err)
			return fmt.Errorf("could not create proxyless dialer: %w", err)
		}
	}

	r.srv = &http.Server{
		Handler: &handler,
		// Prevent slowloris attacks by setting a read timeout.
		ReadHeaderTimeout: 5 * time.Second,
	}

	r.setStatus(true)
	return r.listenAndServe(addr)
}

func (r *Radiance) listenAndServe(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("could not listen on %v: %w", addr, err)
	}

	log.Info("Listening on", "addr", addr)
	return r.srv.Serve(listener)
}

// Shutdown stops the Radiance server.
// This function will be replaced by StopVPN as part of https://github.com/getlantern/engineering/issues/1883
func (r *Radiance) shutdown(ctx context.Context) error {
	if !r.connectionStatus() {
		return nil
	}
	if r.srv == nil {
		return fmt.Errorf("server is nil")
	}
	if err := r.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown server: %w", err)
	}
	r.confHandler.Stop()
	r.setStatus(false)
	close(r.stopChan)
	// Flush sentry events before returning
	if result := sentry.Flush(6 * time.Second); !result {
		log.Error("sentry.Flush: timeout")
	}
	return nil
}

// StartVPN starts the local VPN device, configuring routing rules such that network traffic on this
// machine is sent through this instance of Radiance.
//
// This function will be implemented as part of https://github.com/getlantern/engineering/issues/1883
func (r *Radiance) StartVPN() error {
	log.Debug("Starting VPN")
	err := r.vpnClient.Start()
	r.setStatus(err == nil)
	return err
}

// StopVPN stops the local VPN device and removes routing rules configured by StartVPN.
//
// This function will be implemented as part of https://github.com/getlantern/engineering/issues/1883
func (r *Radiance) StopVPN() error {
	log.Debug("Stopping VPN")
	r.setStatus(false)
	return r.vpnClient.Stop()
}

// PauseVPN pauses the VPN connection for the specified duration.
func (r *Radiance) PauseVPN(dur time.Duration) error {
	log.Info("Pausing VPN for", "duration", dur)
	return r.vpnClient.Pause(dur)
}

// ResumeVPN resumes a paused VPN connection.
func (r *Radiance) ResumeVPN() {
	log.Debug("Resuming VPN")
	r.vpnClient.Resume()
}

func (r *Radiance) connectionStatus() bool {
	r.statusMutex.Lock()
	defer r.statusMutex.Unlock()
	return r.connected
}

func (r *Radiance) setStatus(connected bool) {
	r.statusMutex.Lock()
	r.connected = connected
	r.statusMutex.Unlock()

	// send notifications in a separate goroutine to avoid blocking the Radiance main loop
	go func() {
		// Recover from panics to avoid crashing the Radiance main loop
		defer func() {
			if r := recover(); r != nil {
				log.Error("Recovered from panic", "error", r)
				reporting.PanicListener(fmt.Sprintf("Recovered from panic: %v", r))
			}
		}()
	}()
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
	if !r.connectionStatus() {
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
func (r *Radiance) ReportIssue(email string, report IssueReport) error {
	if report.Type == "" && report.Description == "" {
		return fmt.Errorf("issue report should contain at least type or description")
	}
	// get issue type as integer
	typeInt, ok := issueTypeMap[report.Type]
	if !ok {
		slog.Error("Unknown issue type: %s, set to Other", "type", report.Type)
		typeInt = 9
	}
	// get country from the config returned by the backend
	_, country, err := r.confHandler.GetConfig(eventual.DontWait)
	if err != nil {
		slog.Error("Failed to get country", "error", err)
		country = ""
	}

	return r.issueReporter.Report(
		logDir(),
		email,
		typeInt,
		report.Description,
		report.Attachment,
		report.Device,
		report.Model,
		country)
}

func logDir() string {
	dir := appdir.Logs("Lantern")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// Return an slog logger configured to write to both stdout and the log file.
func newLog(logPath string) *slog.Logger {
	f, err := os.Create(logPath)
	if err != nil {
		return nil
	}
	// defer f.Close() - file should be closed externally when logger is no longer needed
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, f), nil))
	slog.SetDefault(logger)
	return logger
}

// SelectCustomServer configures the client to use the input server. cfg may be nil if
// this server name is already configured for this user. If this server name is not configured
// for this user, it will be added to the list of configured servers.
func (r *Radiance) SelectCustomServer(name string, cfg boxservice.ServerConnectConfig) error {
	// TODO: This function should persist the selected configured servers locally.
	// Since we're not storing configurations locally and don't have a directory
	// this info thill should be implemented in the future.
	r.configuredServersMutex.Lock()
	defer r.configuredServersMutex.Unlock()
	if cfg == nil {
		var exists bool
		if cfg, exists = r.configuredServers[name]; !exists {
			return fmt.Errorf("received a nil config and a not registered server with name %q", name)
		}
	} else {
		r.configuredServers[name] = cfg
	}

	return r.vpnClient.SelectCustomServer(cfg)
}

// DeselectCustomServer configures the client to use its own choice of server assigned
// by the Lantern back-end.
func (r *Radiance) DeselectCustomServer() error {
	return r.vpnClient.DeselectCustomServer()
}
