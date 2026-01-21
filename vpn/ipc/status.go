package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"

	"github.com/sagernet/sing-box/common/conntrack"
	"github.com/sagernet/sing/common/memory"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	StatusInitializing = "initializing"
	StatusConnecting   = "connecting"
	StatusRunning      = "running"
	StatusClosing      = "closing"
	StatusClosed       = "closed"
)

// Metrics represents the runtime metrics of the service.
type Metrics struct {
	Memory      uint64
	Goroutines  int
	Connections int

	// UplinkTotal and DownlinkTotal are only available when the service is running and there are
	// active connections.
	// In bytes.
	UplinkTotal int64
	// In bytes.
	DownlinkTotal int64
}

// GetMetrics retrieves the current runtime metrics of the service.
func GetMetrics(ctx context.Context) (Metrics, error) {
	return sendRequest[Metrics](ctx, "GET", metricsEndpoint, nil)
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer(tracerName).Start(r.Context(), "server.metricsHandler")
	defer span.End()
	stats := Metrics{
		Memory:      memory.Inuse(),
		Goroutines:  runtime.NumGoroutine(),
		Connections: conntrack.Count(),
	}
	if s.service.Status() == StatusRunning {
		up, down := s.service.ClashServer().TrafficManager().Total()
		stats.UplinkTotal, stats.DownlinkTotal = up, down
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

type state struct {
	State string `json:"state"`
}

// GetStatus retrieves the current status of the service.
func GetStatus(ctx context.Context) (string, error) {
	// try to dial first to check if IPC server is even running and avoid waiting for timeout
	if canDial, err := tryDial(ctx); !canDial {
		return StatusClosed, err
	}

	res, err := sendRequest[state](ctx, "GET", statusEndpoint, nil)
	if errors.Is(err, ErrIPCNotRunning) || errors.Is(err, ErrServiceIsNotReady) {
		return StatusClosed, nil
	}
	if err != nil {
		return "", fmt.Errorf("error getting status: %w", err)
	}
	return res.State, nil
}

func tryDial(ctx context.Context) (bool, error) {
	conn, err := dialContext(ctx, "", "")
	if err == nil {
		conn.Close()
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // IPC server is not running so don't treat it as an error
	}
	return false, err
}

func (s *Server) statusHandler(w http.ResponseWriter, r *http.Request) {
	span := trace.SpanFromContext(r.Context())
	status := s.service.Status()
	span.SetAttributes(attribute.String("status", status))
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(state{status}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *Server) GetStatus() string {
	if s.service == nil {
		return StatusClosed
	}
	return s.service.Status()
}
