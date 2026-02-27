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
	"github.com/getlantern/radiance/servers"
)

func TestStatusEventsHandler(t *testing.T) {
	svc := &mockService{status: Disconnected}
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
	status VPNStatus
}

func (m *mockService) Ctx() context.Context                                     { return nil }
func (m *mockService) Status() VPNStatus                                        { return m.status }
func (m *mockService) Start(context.Context, string) error                      { return nil }
func (m *mockService) Restart(context.Context, string) error                    { return nil }
func (m *mockService) ClashServer() *clashapi.Server                            { return nil }
func (m *mockService) Close() error                                             { return nil }
func (m *mockService) UpdateOutbounds(options servers.Servers) error            { return nil }
func (m *mockService) AddOutbounds(group string, options servers.Options) error { return nil }
func (m *mockService) RemoveOutbounds(group string, tags []string) error        { return nil }
