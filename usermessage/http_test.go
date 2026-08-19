package usermessage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	wire "github.com/getlantern/common/usermessage"
	"github.com/getlantern/kindling"

	"github.com/getlantern/radiance/common"
)

func TestHTTPFetcherContractAndCredentials(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/user-messages", r.URL.Path)
		require.Equal(t, "12345", r.Header.Get(common.UserIDHeader))
		require.Equal(t, "secret-token", r.Header.Get(common.ProTokenHeader))
		require.Equal(t, "macos", r.Header.Get(common.PlatformHeader))
		require.Equal(t, "9.2.0", r.Header.Get(common.AppVersionHeader))
		require.Equal(t, "1", r.Header.Get(kindling.IdempotentHeader))

		var request wire.UserMessageRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, wire.CapabilityUserMessagesV1, request.Capability)
		require.Equal(t, "fa-IR", request.Locale)
		require.Equal(t, []string{"seen-1"}, request.SeenDisplayIDs)

		writeWireResponse(t, w, wire.UserMessageResponse{
			PollIntervalSeconds: wire.MaxPollIntervalSeconds,
			Message:             testMessage("display-1", expiresAt),
		})
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(server.Client(), server.URL+"/v1/user-messages")
	response, err := fetcher.Fetch(context.Background(), testClientContext(), []string{"seen-1"})
	require.NoError(t, err)
	require.Equal(t, "display-1", response.Message.DisplayID)
}

func TestHTTPFetcherSafelyIgnoresUnsupportedMessages(t *testing.T) {
	tests := map[string]func(*wire.ResolvedUserMessage){
		"surface": func(message *wire.ResolvedUserMessage) {
			message.Surface = "future_surface"
		},
		"action": func(message *wire.ResolvedUserMessage) {
			message.ButtonLabel = "Act"
			message.Action = &wire.Action{Type: "future_action"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			message := testMessage("display-1", time.Now().Add(time.Hour))
			mutate(message)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeWireResponse(t, w, wire.UserMessageResponse{
					PollIntervalSeconds: 60,
					Message:             message,
				})
			}))
			defer server.Close()

			response, err := NewHTTPFetcher(server.Client(), server.URL).Fetch(
				context.Background(), testClientContext(), nil,
			)
			require.NoError(t, err)
			require.Nil(t, response.Message)
			require.Equal(t, 60, response.PollIntervalSeconds)
		})
	}
}

func TestHTTPFetcherRejectsNonCanonicalUserID(t *testing.T) {
	clientContext := testClientContext()
	clientContext.UserID = "not-a-number"
	_, err := NewHTTPFetcher(http.DefaultClient, "https://example.com").Fetch(
		context.Background(), clientContext, nil,
	)
	require.ErrorIs(t, err, errCredentialsUnavailable)
}

func writeWireResponse(t *testing.T, w http.ResponseWriter, response wire.UserMessageResponse) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(response))
}

func testClientContext() ClientContext {
	return ClientContext{
		UserID:     "12345",
		ProToken:   "secret-token",
		Locale:     "fa-IR",
		Platform:   "macos",
		AppVersion: "9.2.0",
	}
}

func testMessage(displayID string, expiresAt time.Time) *wire.ResolvedUserMessage {
	return &wire.ResolvedUserMessage{
		DisplayID:  displayID,
		CampaignID: "campaign-1",
		RevisionID: "revision-1",
		DeliveryID: "delivery-1",
		Surface:    wire.SurfaceSnackbar,
		Locale:     "fa-IR",
		Body:       "A safe localized message",
		ExpiresAt:  expiresAt,
	}
}
