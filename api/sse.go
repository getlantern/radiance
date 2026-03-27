package api

import (
	"bufio"
	"context"
	"io"
	"strings"
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
