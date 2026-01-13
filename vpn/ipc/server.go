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
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sagernet/sing-box/experimental/clashapi"

	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/traces"
)

var (
	ErrServiceIsNotReady = errors.New("service is not ready")
	ErrIPCNotRunning     = errors.New("IPC not running")
)

// Service defines the interface that the IPC server uses to interact with the underlying VPN service.
type Service interface {
	Ctx() context.Context
	Status() string
	ClashServer() *clashapi.Server
	Close() error
}

type StartFn func(ctx context.Context, group, tag string) (Service, error)

// Server represents the IPC server that communicates over a Unix domain socket for Unix-like
// systems, and a named pipe for Windows.
type Server struct {
	svr       *http.Server
	service   Service
	router    chi.Router
	startFn   StartFn
	mutex     sync.RWMutex
	vpnStatus atomic.Value // string
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
	s.router.Post(closeServiceEndpoint, s.closeServiceHandler)
	s.router.Post(restartServiceEndpoint, s.restartServiceHandler)
	s.router.Post(closeConnectionsEndpoint, s.closeConnectionHandler)
	return s
}

// Start starts the IPC server. The socket file will be created in the "basePath" directory.
// On Windows, the "basePath" is ignored and a default named pipe path is used.
func (s *Server) Start(basePath string, fn StartFn) error {
	s.startFn = fn
	l, err := listen(basePath)
	if err != nil {
		return fmt.Errorf("IPC server: listen: %w", err)
	}
	svr := &http.Server{
		Handler:      s.router,
		ReadTimeout:  time.Second * 5,
		WriteTimeout: time.Second * 5,
	}
	s.svr = svr
	go func() {
		slog.Info("IPC server started", "address", l.Addr().String())
		err := svr.Serve(l)
		if err != nil && err != http.ErrServerClosed {
			slog.Error("IPC server", "error", err)
		}
	}()

	return nil
}

// Close shuts down the IPC server.
func (s *Server) Close() error {
	slog.Info("Closing IPC server")
	return s.svr.Close()
}

// CloseService sends a request to shutdown the service
func CloseService(ctx context.Context) error {
	_, err := sendRequest[empty](ctx, "POST", closeServiceEndpoint, nil)
	return err
}

// StartService sends a request to start the service
func StartService(ctx context.Context, group, tag string) error {
	_, err := sendRequest[empty](ctx, "POST", startServiceEndpoint, selection{GroupTag: group, OutboundTag: tag})
	return err
}

// StopService sends a request to stop the service (IPC server stays up)
func StopService(ctx context.Context) error {
	_, err := sendRequest[empty](ctx, "POST", stopServiceEndpoint, nil)
	return err
}

// SetService updates the service attached to the server and returns the old service, if any.
// Typically called when starting or replacing the VPN tunnel.
func (s *Server) SetService(svc Service) Service {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	old := s.service
	s.service = svc
	return old
}

func (s *Server) startServiceHandler(w http.ResponseWriter, r *http.Request) {
	if s.startFn == nil {
		http.Error(w, "start not supported", http.StatusNotImplemented)
		return
	}
	// check if service is already running
	if s.GetStatus() == StatusRunning {
		w.WriteHeader(http.StatusOK)
		return
	}
	var p selection
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.StartService(r.Context(), p.GroupTag, p.OutboundTag); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) StartService(ctx context.Context, group, tag string) error {
	svc, err := s.startFn(ctx, group, tag)
	if err != nil {
		s.setVPNStatus(ErrorStatus, err)
		return fmt.Errorf("error starting service: %w", err)
	}
	s.setVPNStatus(Connected, nil)
	s.SetService(svc)
	return nil
}

func (s *Server) stopServiceHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.StopService(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) StopService(ctx context.Context) error {
	defer s.setVPNStatus(Disconnected, nil)
	svc := s.SetService(&closedService{})
	if svc != nil {
		if err := svc.Close(); err != nil {
			return fmt.Errorf("error stopping service: %w", err)
		}
	}
	return nil
}

func (s *Server) closeServiceHandler(w http.ResponseWriter, r *http.Request) {
	slog.Info("Received request to close service via IPC")
	svc := s.SetService(&closedService{})
	if err := svc.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.svr.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			traces.RecordError(context.Background(), err)
		}
	}()
}

func RestartService(ctx context.Context) error {
	_, err := sendRequest[empty](ctx, "POST", restartServiceEndpoint, nil)
	return err
}

func (s *Server) RestartService(ctx context.Context) error {
	svc := s.service
	if svc.Status() != StatusRunning {
		return ErrServiceIsNotReady
	}
	mode := svc.ClashServer().Mode()
	groupOutbound, err := getGroupOutbound(svc.Ctx(), mode)
	if err != nil {
		return fmt.Errorf("error getting current outbound group: %w", err)
	}
	selected := groupOutbound.Now()

	if err := s.StopService(ctx); err != nil {
		return fmt.Errorf("error stopping service during restart: %w", err)
	}
	if err := s.StartService(ctx, mode, selected); err != nil {
		return fmt.Errorf("error restarting service: %w", err)
	}
	return nil
}

func (s *Server) restartServiceHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.RestartService(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) setVPNStatus(status VPNStatus, err error) {
	s.vpnStatus.Store(status)
	events.Emit(StatusUpdateEvent{Status: status, Error: err})
}

// closedService is a stub service that always returns "closed" status. It's used to replace the
// actual service when it's being closed, to prevent any new requests from being processed after
// the close request.
type closedService struct {
	Service
}

func (s *closedService) Status() string { return StatusClosed }
func (s *closedService) Close() error   { return nil }
