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
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sagernet/sing-box/experimental/clashapi"
)

var (
	ErrServiceIsNotReady   = errors.New("service is not ready")
	ErrServiceIsNotRunning = errors.New("service is not running")
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
	svr     *http.Server
	service Service

	router  chi.Router
	startFn StartFn
}

// NewServer creates a new Server instance with the provided Service.
func NewServer(service Service) *Server {
	s := &Server{service: service, router: chi.NewMux()}
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
	s.router.Post(closeServiceEndpoint, s.closeServiceHandler)
	s.router.Post(closeConnectionsEndpoint, s.closeConnectionHandler)
	return s
}

// Start starts the IPC server. The socket file will be created in the "basePath" directory.
// On Windows, the "basePath" is ignored and a default named pipe path is used.
func (s *Server) Start(basePath string) error {
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

// SetStartFn registers a function that will be called when the start endpoint is hit
func (s *Server) SetStartFn(fn StartFn) { s.startFn = fn }

// SetService updates the service attached to the server.
// Typically called when starting or replacing the VPN tunnel
func (s *Server) SetService(svc Service) { s.service = svc }

func (s *Server) startServiceHandler(w http.ResponseWriter, r *http.Request) {
	if s.startFn == nil {
		http.Error(w, "start not supported", http.StatusNotImplemented)
		return
	}
	// check if service is already running
	if s.service.Status() == StatusRunning {
		w.WriteHeader(http.StatusOK)
		return
	}
	var p selection
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	svc, err := s.startFn(r.Context(), p.GroupTag, p.OutboundTag)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.SetService(svc)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) closeServiceHandler(w http.ResponseWriter, r *http.Request) {
	service := s.service
	s.service = &closedService{}
	if err := service.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// closedService is a stub service that always returns "closed" status. It's used to replace the
// actual service when it's being closed, to prevent any new requests from being processed after
// the close request.
type closedService struct {
	Service
}

func (s *closedService) Status() string { return StatusClosed }
func (s *closedService) Close() error   { return nil }
