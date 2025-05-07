// Package radiance provides a local server that proxies all requests to a remote proxy server using different
// protocols meant to circumvent censorship. Radiance uses a [transport.StreamDialer] to dial the target server
// over the desired protocol. The [config.Config] is used to configure the dialer for a proxy server.
package radiance

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/api"
	"github.com/getlantern/radiance/client"
	"github.com/getlantern/radiance/common"

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
	client.VPNClient

	confHandler  configHandler
	activeServer *atomic.Value
	stopChan     chan struct{}
	//user config is the user config object that contains the device ID and other user data
	userInfo common.UserInfo

	issueReporter *issue.IssueReporter
	logsDir       string
	shutdownFuncs []func(context.Context) error
	closeOnce     sync.Once
}

// NewRadiance creates a new Radiance VPN client. platIfce is the platform interface used to
// interact with the underlying platform on iOS and Android. On other platforms, it is ignored and
// can be nil.
func NewRadiance(opts client.Options) (*Radiance, error) {
	init, err := initialize(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize common: %w", err)
	}
	shutdownFuncs := []func(context.Context) error{}
	shutdownMetrics, err := metrics.SetupOTelSDK(context.Background())
	if err != nil {
		slog.Error("Failed to setup OpenTelemetry SDK", "error", err)
	} else if shutdownMetrics != nil {
		shutdownFuncs = append(shutdownFuncs, shutdownMetrics)
		slog.Debug("Setup OpenTelemetry SDK", "shutdown", shutdownMetrics)
	}

	vpnC, err := client.NewVPNClient(opts)
	if err != nil {
		slog.Error("Failed to create VPN client", "error", err)
		return nil, fmt.Errorf("failed to create VPN client: %w", err)
	}

	u := api.NewUser(init.kindling.NewHTTPClient(), init.userInfo)
	issueReporter, err := issue.NewIssueReporter(init.kindling.NewHTTPClient(), u, init.userInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to create issue reporter: %w", err)
	}
	cOpts := config.Options{
		PollInterval:     configPollInterval,
		HTTPClient:       init.kindling.NewHTTPClient(),
		User:             init.userInfo,
		DataDir:          opts.DataDir,
		ConfigRespParser: boxservice.UnmarshalConfig,
		Locale:           opts.Locale,
	}
	confHandler := config.NewConfigHandler(cOpts)
	confHandler.AddConfigListener(vpnC.OnNewConfig)

	return &Radiance{
		VPNClient:     vpnC,
		confHandler:   confHandler,
		activeServer:  new(atomic.Value),
		stopChan:      make(chan struct{}),
		userInfo:    init.userInfo,
		issueReporter: issueReporter,
		logsDir:       opts.LogDir,
		shutdownFuncs: shutdownFuncs,
	}, nil
}

// NewAPIHandler creates a new APIHandler instance. This is used to interact with the API.
func NewAPIHandler(opts client.Options) (*api.APIHandler, error) {
	// Note: This separation is necessary because the iOS tunnel runs in a different isolated process.
	// and we need access to the API in the main process.
	init, err := initialize(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize common: %w", err)
	}
	apiHandler := api.NewAPIHandlerInternal(init.kindling.NewHTTPClient(), init.userInfo)
	return apiHandler, nil
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
		r.logsDir,
		email,
		typeInt,
		report.Description,
		report.Attachment,
		report.Device,
		report.Model,
		country)
}

func (r *Radiance) AddCustomServer(cfg boxservice.ServerConnectConfig) error {
	return r.VPNClient.AddCustomServer(cfg)
}

func (r *Radiance) SelectCustomServer(tag string) error {
	return r.VPNClient.SelectCustomServer(tag)
}

func (r *Radiance) RemoveCustomServer(tag string) error {
	return r.VPNClient.RemoveCustomServer(tag)
}

// SplitTunnelHandler returns the split tunnel handler for the VPN client.
func (r *Radiance) SplitTunnelHandler() *client.SplitTunnel {
	return r.VPNClient.SplitTunnelHandler()
}
