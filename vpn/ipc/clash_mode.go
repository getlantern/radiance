package ipc

import (
	"encoding/json"
	"net/http"

	"github.com/getlantern/radiance/traces"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type m struct {
	Mode string `json:"mode"`
}

// GetClashMode retrieves the current mode from the Clash server.
func GetClashMode() (string, error) {
	res, err := sendRequest[m]("GET", clashModeEndpoint, nil)
	if err != nil {
		return "", err
	}
	return res.Mode, nil
}

// SetClashMode sets the mode of the Clash server.
func SetClashMode(mode string) error {
	_, err := sendRequest[empty]("POST", clashModeEndpoint, m{Mode: mode})
	return err
}

// clashModeHandler handles HTTP requests for getting or setting the Clash server mode.
func (s *Server) clashModeHandler(w http.ResponseWriter, req *http.Request) {
	_, span := otel.Tracer(tracerName).Start(req.Context(), "server.clashModeHandler")
	defer span.End()
	if s.service.Status() != StatusRunning {
		http.Error(w, traces.RecordError(span, ErrServiceIsNotReady).Error(), http.StatusServiceUnavailable)
		return
	}
	cs := s.service.ClashServer()
	switch req.Method {
	case "GET":
		mode := cs.Mode()
		span.SetAttributes(attribute.String("mode", mode))
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(m{Mode: mode}); err != nil {
			http.Error(w, traces.RecordError(span, err).Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "POST":
		var mode m
		if err := json.NewDecoder(req.Body).Decode(&mode); err != nil {
			http.Error(w, traces.RecordError(span, err).Error(), http.StatusBadRequest)
			return
		}
		span.SetAttributes(attribute.String("mode", mode.Mode))
		cs.SetMode(mode.Mode)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
