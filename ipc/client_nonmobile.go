//go:build (!android && !ios && !darwin) || (darwin && lanternd)

package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	rlog "github.com/getlantern/radiance/log"
)

// Client communicates with the IPC server over a local socket.
type Client struct {
	http *http.Client
}

// NewClient creates a new IPC client that communicates exclusively through the IPC server.
func NewClient() *Client {
	return newClient()
}

// Close releases resources held by the client, including any local backend.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
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
		return nil, fmt.Errorf("ipc request %s %s: %w", method, endpoint, err)
	}
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

// TailLogs connects to the log stream endpoint and calls handler for each log
// entry received until ctx is cancelled or the connection is closed.
func (c *Client) TailLogs(ctx context.Context, handler func(rlog.LogEntry)) error {
	return c.sseStream(ctx, logsStreamEndpoint, func(data []byte) {
		var entry rlog.LogEntry
		if json.Unmarshal(data, &entry) == nil {
			handler(entry)
		}
	})
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
		if isConnectionError(err) {
			return ErrIPCNotRunning
		}
		return fmt.Errorf("SSE connect %s: %w", endpoint, err)
	}
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
