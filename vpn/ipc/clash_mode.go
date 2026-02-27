package ipc

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/internal"
)

type m struct {
	Mode string `json:"mode"`
}

// GetClashMode retrieves the current mode from the Clash server.
func GetClashMode(ctx context.Context) (string, error) {
	res, err := sendRequest[m](ctx, "GET", clashModeEndpoint, nil)
	if err != nil {
		return "", err
	}
	return res.Mode, nil
}

// SetClashMode sets the mode of the Clash server.
func SetClashMode(ctx context.Context, mode string) error {
	_, err := sendRequest[empty](ctx, "POST", clashModeEndpoint, m{Mode: mode})
	return err
}

// clashModeHandler handles HTTP requests for getting or setting the Clash server mode.
func (s *Server) clashModeHandler(w http.ResponseWriter, req *http.Request) {
	span := trace.SpanFromContext(req.Context())
	if s.service.Status() != Connected {
		http.Error(w, ErrServiceIsNotReady.Error(), http.StatusServiceUnavailable)
		return
	}
	cs := s.service.ClashServer()
	switch req.Method {
	case "GET":
		mode := cs.Mode()
		span.SetAttributes(attribute.String("mode", mode))
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(m{Mode: mode}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "POST":
		var mode m
		if err := json.NewDecoder(req.Body).Decode(&mode); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		span.SetAttributes(attribute.String("mode", mode.Mode))
		slog.Log(nil, internal.LevelTrace, "Setting clash mode", "mode", mode.Mode)
		cs.SetMode(mode.Mode)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
