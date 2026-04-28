package account

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/traces"
)

type sseEvent struct {
	Type string
	Data string
}

// readSSE reads Server-Sent Events from body and sends parsed events on the
// returned channel. The channel is closed when the body returns EOF, an error
// occurs, or ctx is cancelled. The caller is responsible for closing body.
// After the channel is closed, call the returned function to retrieve any
// scanner error (nil on clean EOF).
func readSSE(ctx context.Context, body io.Reader) (<-chan sseEvent, func() error) {
	ch := make(chan sseEvent, 1)
	var scanErr error
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1 MB max token
		var evt sseEvent
		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				evt.Type = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				dataLine := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if evt.Data == "" {
					evt.Data = dataLine
				} else {
					evt.Data = evt.Data + "\n" + dataLine
				}
			case strings.HasPrefix(line, ":"):
				// comment / heartbeat — ignore
			case line == "":
				// blank line = event delimiter
				if evt.Type != "" || evt.Data != "" {
					select {
					case ch <- evt:
					case <-ctx.Done():
						return
					}
					evt = sseEvent{}
				}
			}
		}
		scanErr = scanner.Err()
	}()
	return ch, func() error { return scanErr }
}

// DataCapStream connects to the datacap SSE endpoint and calls handler whenever
// the server pushes an update. The method blocks until ctx is cancelled,
// reconnecting with backoff on stream errors.
func (a *Client) DataCapStream(ctx context.Context, handler func(*DataCapInfo)) error {
	bo := common.NewBackoff(2 * time.Minute)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		start := time.Now()
		err := a.connectDataCapSSE(ctx, handler)
		if err != nil {
			slog.Debug("datacap SSE stream ended", "error", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Reset backoff if the connection was up for a while before dropping,
		// so we reconnect quickly after a transient disconnect.
		if time.Since(start) > 30*time.Second {
			bo.Reset()
		}
		bo.Wait(ctx)
	}
}

// connectDataCapSSE opens an SSE connection to the datacap stream endpoint and
// processes events until the stream ends or ctx is cancelled.
func (a *Client) connectDataCapSSE(ctx context.Context, handler func(*DataCapInfo)) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "datacap_sse")
	defer span.End()

	sseURL := fmt.Sprintf("%s/stream/datacap/%s", a.baseURL(), settings.GetString(settings.DeviceIDKey))
	req, err := common.NewRequestWithHeaders(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("datacap SSE request: %w", err))
	}
	req.Header.Set(common.AcceptHeader, "text/event-stream")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("datacap SSE connect: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return traces.RecordError(ctx, fmt.Errorf("datacap SSE status %d", resp.StatusCode))
	}

	slog.Debug("connected to datacap SSE stream")
	eventCh, scanErr := readSSE(ctx, resp.Body)
	for evt := range eventCh {
		switch evt.Type {
		case "datacap":
			var datacap DataCapInfo
			if err := json.Unmarshal([]byte(evt.Data), &datacap); err != nil {
				slog.Debug("datacap SSE unmarshal error", "error", err)
				continue
			}
			handler(&datacap)
			if datacap.Usage != nil {
				slog.Debug("datacap updated", "bytesUsed", datacap.Usage.BytesUsed)
			}
		case "cap_exhausted":
			slog.Warn("datacap exhausted")
		default:
			// heartbeat or unknown event — ignore
		}
	}
	if err := ctx.Err(); err != nil {
		return traces.RecordError(ctx, err)
	}
	if err := scanErr(); err != nil {
		return traces.RecordError(ctx, fmt.Errorf("datacap SSE scanner: %w", err))
	}
	return traces.RecordError(ctx, errors.New("datacap SSE stream ended unexpectedly"))
}
