package api

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadSSE_BasicEvent(t *testing.T) {
	body := io.NopCloser(strings.NewReader("event: datacap\ndata: {\"enabled\":true}\n\n"))
	ctx := context.Background()
	ch, scanErr := readSSE(ctx, body)

	evt, ok := <-ch
	require.True(t, ok)
	assert.Equal(t, "datacap", evt.Type)
	assert.Equal(t, `{"enabled":true}`, evt.Data)

	// Channel should be closed after EOF.
	_, ok = <-ch
	assert.False(t, ok)
	assert.NoError(t, scanErr())
}

func TestReadSSE_MultipleEvents(t *testing.T) {
	input := "event: datacap\ndata: {\"enabled\":true}\n\nevent: cap_exhausted\ndata: \n\n"
	body := io.NopCloser(strings.NewReader(input))
	ctx := context.Background()
	ch, _ := readSSE(ctx, body)

	evt1 := <-ch
	assert.Equal(t, "datacap", evt1.Type)
	assert.Equal(t, `{"enabled":true}`, evt1.Data)

	evt2 := <-ch
	assert.Equal(t, "cap_exhausted", evt2.Type)
}

func TestReadSSE_MultiLineData(t *testing.T) {
	// Per SSE spec, multiple data: lines are concatenated with \n.
	input := "event: datacap\ndata: line1\ndata: line2\ndata: line3\n\n"
	body := io.NopCloser(strings.NewReader(input))
	ctx := context.Background()
	ch, scanErr := readSSE(ctx, body)

	evt := <-ch
	assert.Equal(t, "datacap", evt.Type)
	assert.Equal(t, "line1\nline2\nline3", evt.Data)

	_, ok := <-ch
	assert.False(t, ok)
	assert.NoError(t, scanErr())
}

func TestReadSSE_HeartbeatIgnored(t *testing.T) {
	// Heartbeat comment followed by a real event.
	input := ": heartbeat\n\nevent: datacap\ndata: {}\n\n"
	body := io.NopCloser(strings.NewReader(input))
	ctx := context.Background()
	ch, _ := readSSE(ctx, body)

	evt := <-ch
	assert.Equal(t, "datacap", evt.Type)
	assert.Equal(t, "{}", evt.Data)
}

func TestReadSSE_ContextCancellation(t *testing.T) {
	// Use a pipe so the reader blocks until we cancel. Closing the writer
	// simulates what the HTTP transport does when the request context is
	// cancelled (the underlying connection is severed, unblocking Read).
	pr, pw := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := readSSE(ctx, pr)

	cancel()
	pw.Close() // unblocks the blocked Read, like HTTP transport would

	// Channel should close promptly.
	select {
	case _, ok := <-ch:
		assert.False(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after context cancellation")
	}
}

func TestReadSSE_EmptyLinesIgnored(t *testing.T) {
	// Multiple blank lines should not produce empty events.
	input := "\n\n\nevent: datacap\ndata: ok\n\n"
	body := io.NopCloser(strings.NewReader(input))
	ctx := context.Background()
	ch, _ := readSSE(ctx, body)

	evt := <-ch
	assert.Equal(t, "datacap", evt.Type)
	assert.Equal(t, "ok", evt.Data)

	_, ok := <-ch
	assert.False(t, ok)
}

func TestReadSSE_ScannerError(t *testing.T) {
	// Feed a line longer than the scanner buffer to trigger ErrTooLong.
	// Default scanner buffer is 64KB; our readSSE uses 1MB max.
	// Create a line just over 1MB to trigger the error.
	longLine := "data: " + strings.Repeat("x", 1024*1024+1) + "\n\n"
	body := io.NopCloser(strings.NewReader(longLine))
	ctx := context.Background()
	ch, scanErr := readSSE(ctx, body)

	// Drain the channel.
	for range ch {
	}

	// Scanner should have errored.
	assert.Error(t, scanErr())
}
