package ipc

import (
	"encoding/json"
	"net/http"
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
	if s.service.Status() != StatusRunning {
		http.Error(w, "service not ready", http.StatusServiceUnavailable)
		return
	}
	cs := s.service.ClashServer()
	switch req.Method {
	case "GET":
		mode := cs.Mode()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m{Mode: mode})
	case "POST":
		var mode m
		err := json.NewDecoder(req.Body).Decode(&mode)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cs.SetMode(mode.Mode)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
