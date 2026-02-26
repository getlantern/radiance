package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/experimental/clashapi"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/events"
)

func TestStatusEventsHandler(t *testing.T) {
	svc := newMockService()
	s := &Server{service: svc}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", statusEventsEndpoint, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.statusEventsHandler(rec, req)
	}()

	waitAssert := func(want StatusUpdateEvent, msg string) {
		require.Eventually(t, func() bool {
			return strings.Contains(rec.Body.String(), "\r\n")
		}, time.Second, 10*time.Millisecond, msg)
		evt := parseEventLine(t, rec.Body)
		rec.Body.Reset()
		assert.Equal(t, want, evt, msg)
	}
	waitAssert(StatusUpdateEvent{Status: Disconnected}, "initial event not received")

	// Emit a status change and wait for it to arrive.
	evt := StatusUpdateEvent{Status: Connected}
	events.Emit(evt)
	waitAssert(evt, "connected event not received")

	// Emit an error status
	evt = StatusUpdateEvent{Status: ErrorStatus, Error: "something went wrong"}
	events.Emit(evt)
	waitAssert(evt, "error event not received")

	// Cancel the service context — handler should return.
	svc.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		require.Fail(t, "handler did not return after service context cancellation")
	}
}

func parseEventLine(t *testing.T, body *bytes.Buffer) StatusUpdateEvent {
	line, err := body.ReadBytes('\n')
	require.NoError(t, err)

	var evt StatusUpdateEvent
	line = bytes.TrimSpace(line)
	require.NoError(t, json.Unmarshal(line, &evt))
	return evt
}

type mockService struct {
	ctx    context.Context
	cancel context.CancelFunc
	status VPNStatus
}

func newMockService() *mockService {
	ctx, cancel := context.WithCancel(context.Background())
	return &mockService{ctx: ctx, cancel: cancel, status: Disconnected}
}

func (m *mockService) Ctx() context.Context                        { return m.ctx }
func (m *mockService) Status() VPNStatus                           { return m.status }
func (m *mockService) Start(context.Context, string, string) error { return nil }
func (m *mockService) Restart(context.Context) error               { return nil }
func (m *mockService) ClashServer() *clashapi.Server               { return nil }
func (m *mockService) Close() error                                { m.cancel(); return nil }
