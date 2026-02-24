// Package ipc implements the IPC server for communicating between the client and the VPN service.
// It provides HTTP endpoints for retrieving statistics, managing groups, selecting outbounds,
// changing modes, and closing connections.
package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"go.opentelemetry.io/otel"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/servers"
)

var (
	ErrServiceIsNotReady = errors.New("service is not ready")
	ErrIPCNotRunning     = errors.New("IPC not running")
)

// Service defines the interface that the IPC server uses to interact with the underlying VPN service.
type Service interface {
	Ctx() context.Context
	Status() VPNStatus
	Start(ctx context.Context, options string) error
	Restart(ctx context.Context, options string) error
	Close() error
	ClashServer() *clashapi.Server
	UpdateOutbounds(options servers.Servers) error
	AddOutbounds(group string, options servers.Options) error
	RemoveOutbounds(group string, tags []string) error
}

// Server represents the IPC server that communicates over a Unix domain socket for Unix-like
// systems, and a named pipe for Windows.
type Server struct {
	svr       *http.Server
	service   Service
	router    chi.Router
	vpnStatus atomic.Value // string
	closed    atomic.Bool
}

// StatusUpdateEvent is emitted when the VPN status changes.
type StatusUpdateEvent struct {
	events.Event
	Status VPNStatus
	Error  error
}

type VPNStatus string

// Possible VPN statuses
const (
	Connected     VPNStatus = "connected"
	Disconnected  VPNStatus = "disconnected"
	Connecting    VPNStatus = "connecting"
	Disconnecting VPNStatus = "disconnecting"
	ErrorStatus   VPNStatus = "error"
)

func (vpn *VPNStatus) String() string {
	return string(*vpn)
}

// NewServer creates a new Server instance with the provided Service.
func NewServer(service Service) *Server {
	s := &Server{
		service: service,
		router:  chi.NewMux(),
	}
	s.vpnStatus.Store(Disconnected)
	s.router.Use(log, tracer)

	// Only add auth middleware if not running on mobile, since mobile platforms have their own
	// sandboxing and permission models.
	addAuth := !common.IsMobile() && !_testing
	if addAuth {
		s.router.Use(authPeer)
	}

	s.router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.router.Get(statusEndpoint, s.statusHandler)
	s.router.Get(metricsEndpoint, s.metricsHandler)
	s.router.Get(groupsEndpoint, s.groupHandler)
	s.router.Get(connectionsEndpoint, s.connectionsHandler)
	s.router.Get(selectEndpoint, s.selectedHandler)
	s.router.Get(activeEndpoint, s.activeOutboundHandler)
	s.router.Post(selectEndpoint, s.selectHandler)
	s.router.Get(clashModeEndpoint, s.clashModeHandler)
	s.router.Post(clashModeEndpoint, s.clashModeHandler)
	s.router.Post(startServiceEndpoint, s.startServiceHandler)
	s.router.Post(stopServiceEndpoint, s.stopServiceHandler)
	s.router.Post(restartServiceEndpoint, s.restartServiceHandler)
	s.router.Post(updateOutboundsEndpoint, s.updateOutboundsHandler)
	s.router.Post(addOutboundsEndpoint, s.addOutboundsHandler)
	s.router.Post(removeOutboundsEndpoint, s.removeOutboundsHandler)
	s.router.Post(closeConnectionsEndpoint, s.closeConnectionHandler)

	svr := &http.Server{
		Handler:      s.router,
		ReadTimeout:  time.Second * 5,
		WriteTimeout: time.Second * 5,
	}
	if addAuth {
		svr.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
			peer, err := getConnPeer(c)
			if err != nil {
				slog.Error("Failed to get peer credentials", "error", err)
			}
			return contextWithUsr(ctx, peer)
		}
	}
	s.svr = svr
	return s
}

// Start begins listening for incoming IPC requests.
func (s *Server) Start() error {
	if s.closed.Load() {
		return errors.New("IPC server is closed")
	}
	l, err := listen()
	if err != nil {
		return fmt.Errorf("IPC server: listen: %w", err)
	}
	go func() {
		slog.Info("IPC server started", "address", l.Addr().String())
		err := s.svr.Serve(l)
		if err != nil && err != http.ErrServerClosed {
			slog.Error("IPC server", "error", err)
		}
		s.closed.Store(true)
		if s.service.Status() != Disconnected {
			slog.Warn("IPC server stopped unexpectedly, closing service")
			s.service.Close()
			s.setVPNStatus(ErrorStatus, errors.New("IPC server stopped unexpectedly"))
		}
	}()

	return nil
}

// Close shuts down the IPC server.
func (s *Server) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	defer s.service.Close()

	slog.Info("Closing IPC server")
	return s.svr.Close()
}

func (s *Server) IsClosed() bool {
	return s.closed.Load()
}

type opts struct {
	Options string `json:"options"`
}

// StartService sends a request to start the service
func StartService(ctx context.Context, options string) error {
	_, err := sendRequest[empty](ctx, "POST", startServiceEndpoint, opts{Options: options})
	return err
}

func (s *Server) startServiceHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(tracerName).Start(r.Context(), "ipc.Server.StartService")
	defer span.End()
	switch s.service.Status() {
	case Disconnected:
		// proceed to start
	case Connected:
		w.WriteHeader(http.StatusOK)
		return
	case Disconnecting:
		http.Error(w, "service is disconnecting, please wait", http.StatusConflict)
		return
	default:
		http.Error(w, "service is already starting", http.StatusConflict)
		return
	}
	var p opts
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.setVPNStatus(Connecting, nil)
	if err := s.service.Start(ctx, p.Options); err != nil {
		s.setVPNStatus(ErrorStatus, err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.setVPNStatus(Connected, nil)
	w.WriteHeader(http.StatusOK)
}

// StopService sends a request to stop the service (IPC server stays up)
func StopService(ctx context.Context) error {
	_, err := sendRequest[empty](ctx, "POST", stopServiceEndpoint, nil)
	return err
}

func (s *Server) stopServiceHandler(w http.ResponseWriter, r *http.Request) {
	slog.Debug("Received request to stop service via IPC")
	s.setVPNStatus(Disconnecting, nil)
	if err := s.service.Close(); err != nil {
		s.setVPNStatus(ErrorStatus, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.setVPNStatus(Disconnected, nil)
	w.WriteHeader(http.StatusOK)
}

func RestartService(ctx context.Context, options string) error {
	_, err := sendRequest[empty](ctx, "POST", restartServiceEndpoint, opts{Options: options})
	return err
}

func (s *Server) restartServiceHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer(tracerName).Start(r.Context(), "ipc.Server.restartServiceHandler")
	defer span.End()

	if s.service.Status() != Connected {
		http.Error(w, ErrServiceIsNotReady.Error(), http.StatusInternalServerError)
		return
	}
	var p opts
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.vpnStatus.Store(Disconnected)
	if err := s.service.Restart(ctx, p.Options); err != nil {
		s.setVPNStatus(ErrorStatus, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.setVPNStatus(Connected, nil)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) setVPNStatus(status VPNStatus, err error) {
	s.vpnStatus.Store(status)
	events.Emit(StatusUpdateEvent{Status: status, Error: err})
}
