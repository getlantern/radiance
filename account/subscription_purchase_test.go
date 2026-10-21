package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/log"
)

func TestPurchaseResponseUnmarshalPlan(t *testing.T) {
	var fromString PurchaseResponse
	require.NoError(t, json.Unmarshal([]byte(`{"status":"ok","provider":"web-license","plan":"1y-usd-10","isPro":true}`), &fromString))
	require.Equal(t, "ok", fromString.Status)
	require.Equal(t, "1y-usd-10", fromString.Plan.Id)

	var fromObject PurchaseResponse
	require.NoError(t, json.Unmarshal([]byte(`{"status":"ok","paymentStatus":"done","plan":{"id":"1y-usd-10","usdPrice":1000}}`), &fromObject))
	require.Equal(t, "done", fromObject.PaymentStatus)
	require.Equal(t, "1y-usd-10", fromObject.Plan.Id)
	require.EqualValues(t, 1000, fromObject.Plan.UsdPrice)

	var withError PurchaseResponse
	require.NoError(t, json.Unmarshal([]byte(`{"error":"bad code","errorId":"reseller-codes"}`), &withError))
	require.NotNil(t, withError.BaseResponse)
	require.Equal(t, "bad code", withError.Error)
}

func TestActivationCodeRequest(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		provider string
		field    string
	}{
		{"reseller code", "ABCD-1234", "reseller-code", "resellerCode"},
		{"pro prefix", "PRO-12345-67890", "web-license", "licenseKey"},
		{"mixed case with leading whitespace", "  pRo-12345", "web-license", "licenseKey"},
		{"pro prefix and suffix", "PRO-12345PRO", "web-license", "licenseKey"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/purchase", r.URL.Path)
				require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			}))
			defer srv.Close()

			a := &Client{httpClient: srv.Client(), logger: log.NoOpLogger(), proURL: srv.URL}
			resp, err := a.ActivationCode(context.Background(), "user@example.com", tc.code)
			require.NoError(t, err)
			require.Equal(t, "ok", resp.Status)

			require.Equal(t, tc.provider, got["provider"])
			require.Equal(t, tc.code, got[tc.field], "code must be forwarded unchanged")
			require.Equal(t, "user@example.com", got["email"])
			for _, other := range []string{"licenseKey", "resellerCode"} {
				if other != tc.field {
					require.NotContains(t, got, other)
				}
			}
		})
	}
}
