package ipc

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

const (
	peerStartEndpoint  = "/peer/start"
	peerStopEndpoint   = "/peer/stop"
	peerStatusEndpoint = "/peer/status"
)

// PeerController is implemented by the peer proxy manager.
// The IPC server calls these methods in response to client requests.
type PeerController interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Active() bool
}

// PeerStatus is the response from the peer status endpoint.
type PeerStatus struct {
	Active bool `json:"active"`
}

// SetPeerController registers the peer proxy controller with the IPC server.
// Must be called before Start(). If not called, start/stop return 501 and
// status returns {active: false}.
func (s *Server) SetPeerController(pc PeerController) {
	s.peerController = pc
}

// --- Client-side functions ---

// StartPeerProxy requests the IPC server to start the peer proxy.
func StartPeerProxy(ctx context.Context) error {
	_, err := sendRequest[empty](ctx, "POST", peerStartEndpoint, nil)
	return err
}

// StopPeerProxy requests the IPC server to stop the peer proxy.
func StopPeerProxy(ctx context.Context) error {
	_, err := sendRequest[empty](ctx, "POST", peerStopEndpoint, nil)
	return err
}

// GetPeerStatus returns the current peer proxy status.
func GetPeerStatus(ctx context.Context) (*PeerStatus, error) {
	return sendRequest[*PeerStatus](ctx, "GET", peerStatusEndpoint, nil)
}

// --- Server-side handlers ---

func (s *Server) peerStartHandler(w http.ResponseWriter, r *http.Request) {
	if s.peerController == nil {
		http.Error(w, "peer proxy not available", http.StatusNotImplemented)
		return
	}
	slog.Info("IPC: starting peer proxy")

	// Process asynchronously — starting the peer proxy involves UPnP discovery
	// and API registration which can take several seconds. Use the service
	// context (not the request context, which is canceled when the handler returns).
	w.WriteHeader(http.StatusAccepted)
	go func() {
		if err := s.peerController.Start(s.service.Ctx()); err != nil {
			slog.Error("Failed to start peer proxy", "error", err)
		}
	}()
}

func (s *Server) peerStopHandler(w http.ResponseWriter, r *http.Request) {
	if s.peerController == nil {
		http.Error(w, "peer proxy not available", http.StatusNotImplemented)
		return
	}
	ctx := r.Context()
	slog.Info("IPC: stopping peer proxy")

	if err := s.peerController.Stop(ctx); err != nil {
		slog.Error("Failed to stop peer proxy", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) peerStatusHandler(w http.ResponseWriter, r *http.Request) {
	if s.peerController == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(PeerStatus{Active: false})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(PeerStatus{
		Active: s.peerController.Active(),
	})
}
