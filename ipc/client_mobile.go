//go:build android || ios || (darwin && !standalone)

package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common/settings"
	rlog "github.com/getlantern/radiance/log"
)

type Client struct {
	http     *http.Client
	localapi *localapi
	mu       sync.RWMutex
}

func NewClient(ctx context.Context, opts backend.Options) (*Client, error) {
	b, err := backend.NewLocalBackend(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("create local backend: %w", err)
	}
	b.Start()
	c := newClient()
	c.localapi = newLocalAPI(b, false)
	return c, nil
}

// Close releases resources held by the client, including any local backend.
func (c *Client) Close() {
	c.stopLocal()
	c.http.CloseIdleConnections()
}

func (c *Client) stopLocal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if be := c.localapi.setBackend(nil); be != nil {
		be.Close()
	}
}

// do executes an HTTP request with an optional JSON body and returns the raw response body. If
// body needs to be marshaled using sing/json, it should be pre-marshaled to []byte before passing
// to do. do returns an error if the response status is >= 400.
func (c *Client) do(ctx context.Context, method, endpoint string, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		switch body := body.(type) {
		case []byte:
			bodyReader = bytes.NewReader(body)
		default:
			data, err := json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("marshal request: %w", err)
			}
			bodyReader = bytes.NewReader(data)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, apiURL+endpoint, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if isConnectionError(err) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if be := c.localapi.be.Load(); be == nil {
				opts := backend.Options{
					DataDir:          settings.GetString(settings.DataPathKey),
					LogDir:           settings.GetString(settings.LogPathKey),
					Locale:           settings.GetString(settings.LocaleKey),
					DeviceID:         settings.GetString(settings.DeviceIDKey),
					LogLevel:         settings.GetString(settings.LogLevelKey),
					TelemetryConsent: settings.GetBool(settings.TelemetryKey),
				}
				be, err = backend.NewLocalBackend(ctx, opts)
				if err != nil {
					return nil, fmt.Errorf("create local backend: %w", err)
				}
				c.localapi.setBackend(be)
			}
			if br, ok := bodyReader.(*bytes.Reader); ok {
				br.Seek(0, io.SeekStart)
			}
			req, _ = http.NewRequestWithContext(ctx, method, apiURL+endpoint, bodyReader)
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			return c.doLocal(req)
		}
		return nil, fmt.Errorf("ipc request %s %s: %w", method, endpoint, err)
	}
	c.stopLocal() // IPC is reachable; shut down local backend if still running
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{
			Status:  resp.StatusCode,
			Message: strings.TrimSpace(string(respBody)),
		}
	}
	return respBody, nil
}

// doLocal serves the request through the given in-process handler.
func (c *Client) doLocal(req *http.Request) ([]byte, error) {
	rec := httptest.NewRecorder()
	c.localapi.ServeHTTP(rec, req)

	body := rec.Body.Bytes()
	if rec.Code >= 400 {
		return nil, &Error{
			Status:  rec.Code,
			Message: strings.TrimSpace(string(body)),
		}
	}
	return body, nil
}

// TailLogs connects to the log stream endpoint and calls handler for each log
// entry received until ctx is cancelled or the connection is closed.
func (c *Client) TailLogs(ctx context.Context, handler func(rlog.LogEntry)) error {
	merged := make(chan rlog.LogEntry, 64)

	// Always tail local logs.
	localCh, unsub := rlog.Subscribe()
	defer unsub()
	go func() {
		for {
			select {
			case entry := <-localCh:
				select {
				case merged <- entry:
				default:
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Tail server logs whenever the IPC server is reachable.
	go func() {
		for ctx.Err() == nil {
			c.sseStream(ctx, logsStreamEndpoint, func(data []byte) {
				var entry rlog.LogEntry
				if json.Unmarshal(data, &entry) == nil {
					select {
					case merged <- entry:
					default:
					}
				}
			})
			// Server unavailable or disconnected; wait before retrying.
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case entry := <-merged:
			handler(entry)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sseStream connects to an SSE endpoint and calls handler for each event data line.
// Blocks until ctx is cancelled or the connection is closed.
func (c *Client) sseStream(ctx context.Context, endpoint string, handler func([]byte)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+endpoint, nil)
	if err != nil {
		return fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		c.mu.RLock()
		hasFallback := c.localapi != nil
		c.mu.RUnlock()
		if hasFallback && isConnectionError(err) {
			return ErrIPCNotRunning
		}
		return fmt.Errorf("SSE connect %s: %w", endpoint, err)
	}
	c.stopLocal() // IPC is reachable; shut down local backend if still running
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(body))}
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			handler([]byte(data))
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("SSE %s: read: %w", endpoint, err)
	}
	return nil
}
