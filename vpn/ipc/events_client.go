package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/getlantern/radiance/events"
)

// StartStatusStream starts streaming status updates from the server and emits received
// [StatusUpdateEvent] events until the context is cancelled. If waitForConnect is true, it
// polls in a background goroutine until the server is reachable. When the stream is lost
// (server restart, network error, clean EOF), a [StatusUpdateEvent] with [Disconnected] status
// is emitted. The retry loop continues until a connection is established, the context is cancelled,
// or a non-recoverable error occurs (e.g. connection refused, invalid response).
func StartStatusStream(ctx context.Context, waitForConnect bool) error {
	if !waitForConnect {
		return startStream(ctx)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
				serverListening, err := tryDial(ctx)
				if err != nil {
					events.Emit(StatusUpdateEvent{
						Status: ErrorStatus,
						Error:  fmt.Sprintf("connection error: %v", err),
					})
					return
				}
				if !serverListening {
					continue // we started trying to connect before the server is ready
				}
				err = startStream(ctx)
				if ctx.Err() != nil {
					return
				}
				evt := StatusUpdateEvent{Status: Disconnected}
				if err != nil {
					slog.Warn("status stream disconnected", "error", err)
					evt.Error = fmt.Sprintf("stream disconnected: %v", err)
				}
				// Stream ended cleanly (EOF) — server likely shut down.
				events.Emit(evt)
				return
			}
		}
	}()
	return nil
}

func startStream(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL+statusEventsEndpoint, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: dialContext,
			Protocols:   protocols,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var evt StatusUpdateEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		events.Emit(evt)
	}
	return scanner.Err()
}
