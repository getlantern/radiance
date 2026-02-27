package ipc

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/getlantern/radiance/events"
)

// StatusUpdateEvent is emitted when the VPN status changes.
type StatusUpdateEvent struct {
	events.Event
	Status VPNStatus `json:"status"`
	Error  string    `json:"error,omitempty"`
}

func (s *Server) statusEventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan StatusUpdateEvent, 8)

	// Send the current status immediately so the client doesn't have to wait for a change.
	ch <- StatusUpdateEvent{Status: s.service.Status()}

	sub := events.Subscribe(func(evt StatusUpdateEvent) {
		select {
		case ch <- evt:
		default: // drop if client is slow
		}
	})
	defer sub.Unsubscribe()

	for {
		select {
		case evt := <-ch:
			buf, err := json.Marshal(evt)
			if err != nil {
				slog.Error("failed to marshal event", "error", err)
				continue
			}
			fmt.Fprintf(w, "%s\r\n", buf)
			flusher.Flush()
		case <-r.Context().Done():
			slog.Debug("client disconnected")
			return
		}
	}
}
