package ipc

import (
	"encoding/json"
	"net/http"
	"runtime"

	"github.com/getlantern/radiance/traces"
	"github.com/sagernet/sing-box/common/conntrack"
	"github.com/sagernet/sing/common/memory"
	"go.opentelemetry.io/otel"
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
func GetMetrics() (Metrics, error) {
	return sendRequest[Metrics]("GET", metricsEndpoint, nil)
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

	w.WriteHeader(http.StatusOK)
}

type state struct {
	State string `json:"state"`
}

// GetStatus retrieves the current status of the service.
func GetStatus() (string, error) {
	res, err := sendRequest[state]("GET", statusEndpoint, nil)
	if err != nil {
		return "", err
	}
	return res.State, nil
}

func (s *Server) statusHandler(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer(tracerName).Start(r.Context(), "server.statusHandler")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(state{s.service.Status()}); err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
