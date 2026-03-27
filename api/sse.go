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
func readSSE(ctx context.Context, body io.Reader) <-chan sseEvent {
	ch := make(chan sseEvent, 1)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(body)
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
				evt.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
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
	}()
	return ch
}
