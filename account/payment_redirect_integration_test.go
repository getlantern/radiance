//go:build integration
// +build integration

package account

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
)

const (
	runStagingPaymentRedirectEnv  = "RADIANCE_RUN_STAGING_PAYMENT_REDIRECT"
	paymentRedirectPlanIDEnv      = "RADIANCE_PAYMENT_REDIRECT_PLAN_ID"
	paymentRedirectChannelEnv     = "RADIANCE_PAYMENT_REDIRECT_CHANNEL"
	paymentRedirectProviderEnv    = "RADIANCE_PAYMENT_REDIRECT_PROVIDER"
	paymentRedirectEmailEnv       = "RADIANCE_PAYMENT_REDIRECT_EMAIL"
	paymentRedirectBillingTypeEnv = "RADIANCE_PAYMENT_REDIRECT_BILLING_TYPE"
)

func TestStagingPaymentRedirectIdempotency(t *testing.T) {
	requireStagingPaymentRedirectOptIn(t)

	require.NoError(t, settings.InitSettings(t.TempDir()))
	t.Cleanup(settings.Reset)

	deviceName := "radiance-staging-payment-redirect-" + uniquePaymentRedirectSuffix(t)
	require.NoError(t, settings.Set(settings.DeviceIDKey, deviceName))
	require.NoError(t, settings.Set(settings.LocaleKey, "en-US"))

	client := NewClient(&http.Client{Timeout: 30 * time.Second}, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	planID := stagingPaymentRedirectPlanID(ctx, t, client)
	provider := envOrDefault(paymentRedirectProviderEnv, "stripe")
	email := envOrDefault(
		paymentRedirectEmailEnv,
		fmt.Sprintf("radiance-payment-redirect-ci+%d@getlantern.org", time.Now().Unix()),
	)
	billingType := SubscriptionType(envOrDefault(
		paymentRedirectBillingTypeEnv,
		string(SubscriptionTypeSubscription),
	))

	tests := []struct {
		name string
		call func(context.Context, PaymentRedirectData) (string, error)
		data PaymentRedirectData
	}{
		{
			name: "payment_redirect",
			call: client.PaymentRedirect,
			data: PaymentRedirectData{
				Provider:   provider,
				Plan:       planID,
				DeviceName: deviceName,
				Email:      email,
			},
		},
		{
			name: "subscription_payment_redirect_url",
			call: client.SubscriptionPaymentRedirectURL,
			data: PaymentRedirectData{
				Provider:    provider,
				Plan:        planID,
				DeviceName:  deviceName,
				Email:       email,
				BillingType: billingType,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			firstKey := "radiance-ci-" + tt.name + "-" + uniquePaymentRedirectSuffix(t)
			secondKey := "radiance-ci-" + tt.name + "-" + uniquePaymentRedirectSuffix(t)

			first := tt.data
			first.IdempotencyKey = firstKey
			firstURL, err := tt.call(ctx, first)
			require.NoError(t, err)
			requireHTTPSRedirect(t, firstURL)

			sameKey := tt.data
			sameKey.IdempotencyKey = firstKey
			sameKeyURL, err := tt.call(ctx, sameKey)
			require.NoError(t, err)
			requireHTTPSRedirect(t, sameKeyURL)
			if sameKeyURL != firstURL {
				t.Fatalf("same idempotency key returned a different redirect URL")
			}

			differentKey := tt.data
			differentKey.IdempotencyKey = secondKey
			differentKeyURL, err := tt.call(ctx, differentKey)
			require.NoError(t, err)
			requireHTTPSRedirect(t, differentKeyURL)
			if differentKeyURL == firstURL {
				t.Fatalf("different idempotency key returned the same redirect URL")
			}
		})
	}
}

func requireStagingPaymentRedirectOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv(runStagingPaymentRedirectEnv) != "1" {
		t.Skipf("set %s=1 to run staging payment redirect contract test", runStagingPaymentRedirectEnv)
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RADIANCE_ENV"))) {
	case "stage", "staging":
	default:
		t.Fatalf("RADIANCE_ENV must be stage or staging for this test")
	}
}

func stagingPaymentRedirectPlanID(ctx context.Context, t *testing.T, client *Client) string {
	t.Helper()
	if planID := strings.TrimSpace(os.Getenv(paymentRedirectPlanIDEnv)); planID != "" {
		return planID
	}

	channel := envOrDefault(paymentRedirectChannelEnv, "non-store")
	rawPlans, err := client.SubscriptionPlans(ctx, channel)
	require.NoError(t, err)

	var plans SubscriptionPlans
	require.NoError(t, json.Unmarshal([]byte(rawPlans), &plans))
	for _, plan := range plans.Plans {
		if plan.GetId() != "" {
			return plan.GetId()
		}
	}
	t.Fatalf("staging plans response for channel %q did not include any plan IDs", channel)
	return ""
}

func requireHTTPSRedirect(t *testing.T, raw string) {
	t.Helper()
	u, err := url.Parse(strings.TrimSpace(raw))
	require.NoError(t, err)
	if u.Scheme != "https" || u.Host == "" {
		t.Fatalf("redirect URL must be absolute HTTPS")
	}
}

func uniquePaymentRedirectSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
