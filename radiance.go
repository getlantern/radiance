// Package radiance provides a local server that proxies all requests to a remote proxy server using different
// protocols meant to circumvent censorship. Radiance uses a [transport.StreamDialer] to dial the target server
// over the desired protocol. The [config.Config] is used to configure the dialer for a proxy server.
package radiance

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/experimental/libbox"

	"github.com/getlantern/appdir"
	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/kindling"

	"github.com/getlantern/radiance/client"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/issue"
	"github.com/getlantern/radiance/user"
)

var (
	vpnLogOutput = filepath.Join(logDir(), "lantern.log")
	log          *slog.Logger

	configPollInterval = 10 * time.Minute
)

//go:generate mockgen -destination=radiance_mock_test.go -package=radiance github.com/getlantern/radiance httpServer,configHandler

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

	confHandler  configHandler
	activeConfig *atomic.Value

	connected   bool
	statusMutex sync.Locker
	stopChan    chan struct{}

	user *user.User

	issueReporter *issue.IssueReporter
}

// NewRadiance creates a new Radiance VPN client. platIfce is the platform interface used to
// interact with the underlying platform on iOS and Android. On other platforms, it is ignored and
// can be nil.
func NewRadiance(platIfce libbox.PlatformInterface) (*Radiance, error) {
	var err error
	log, err = newLog(vpnLogOutput)
	if err != nil {
		return nil, fmt.Errorf("could not create log: %w", err)
	}

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
	if runtime.GOOS == "android" {
		//To avoid panic from appDir
		// need to set home dir
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		appdir.SetHomeDir(homeDir)
	}
	dir := appdir.Logs("Lantern")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// Return an slog logger configured to write to both stdout and the log file.
func newLog(logPath string) (*slog.Logger, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	// defer f.Close() - file should be closed externally when logger is no longer needed
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, f), nil))
	slog.SetDefault(logger)
	return logger, nil
}
