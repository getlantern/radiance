package account

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionPaymentRedirect(t *testing.T) {
	ac, ts := newTestClient(t)
	data := PaymentRedirectData{
		Provider:       "stripe",
		Plan:           "pro",
		DeviceName:     "test-device",
		Email:          "",
		BillingType:    SubscriptionTypeOneTime,
		IdempotencyKey: "subscription-redirect-key",
	}
	url, err := ac.SubscriptionPaymentRedirectURL(context.Background(), data)
	require.NoError(t, err)
	assert.NotEmpty(t, url)
	assert.Equal(t, data.IdempotencyKey, ts.subscriptionPaymentRedirectIdempotencyKey)
}

func TestPaymentRedirect(t *testing.T) {
	ac, ts := newTestClient(t)
	data := PaymentRedirectData{
		Provider:       "stripe",
		Plan:           "pro",
		DeviceName:     "test-device",
		Email:          "",
		IdempotencyKey: "payment-redirect-key",
	}
	url, err := ac.PaymentRedirect(context.Background(), data)
	require.NoError(t, err)
	assert.NotEmpty(t, url)
	assert.Equal(t, data.IdempotencyKey, ts.paymentRedirectIdempotencyKey)
}

func TestPaymentRedirectOmitsEmptyIdempotencyKey(t *testing.T) {
	ac, ts := newTestClient(t)
	data := PaymentRedirectData{
		Provider:   "stripe",
		Plan:       "pro",
		DeviceName: "test-device",
		Email:      "",
	}

	url, err := ac.PaymentRedirect(context.Background(), data)
	require.NoError(t, err)
	assert.NotEmpty(t, url)
	assert.False(t, ts.paymentRedirectHasIdempotencyKey)

	url, err = ac.SubscriptionPaymentRedirectURL(context.Background(), data)
	require.NoError(t, err)
	assert.NotEmpty(t, url)
	assert.False(t, ts.subscriptionPaymentRedirectHasIdempotencyKey)
}

func TestPaymentRedirectRequiresRedirectURL(t *testing.T) {
	ac, ts := newTestClient(t)
	ts.paymentRedirectResponse = map[string]string{"status": "error", "error": "try again later"}

	url, err := ac.PaymentRedirect(context.Background(), PaymentRedirectData{
		Provider:   "stripe",
		Plan:       "pro",
		DeviceName: "test-device",
	})
	require.Error(t, err)
	assert.Empty(t, url)
}

func TestNewUser(t *testing.T) {
	ac, _ := newTestClient(t)
	resp, err := ac.NewUser(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestVerifySubscription(t *testing.T) {
	ac, _ := newTestClient(t)
	data := map[string]string{
		"email":  "test@getlantern.org",
		"planID": "1y-usd-10",
	}
	resp, err := ac.VerifySubscription(context.Background(), AppleService, data)
	require.NoError(t, err)
	assert.NotEmpty(t, resp)
}

func TestRestoreSubscription(t *testing.T) {
	t.Run("apple", func(t *testing.T) {
		ac, _ := newTestClient(t)
		resp, err := ac.RestoreSubscription(context.Background(), AppleService, map[string]string{"receipt": "r"})
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, "active", resp.Status)
		assert.Equal(t, int64(1234), resp.ActualUserID)
		assert.Equal(t, "token", resp.ActualUserToken)
	})

	t.Run("google", func(t *testing.T) {
		ac, _ := newTestClient(t)
		resp, err := ac.RestoreSubscription(context.Background(), GoogleService, map[string]string{"purchaseToken": "t"})
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, "active", resp.Status)
	})

	t.Run("unsupported", func(t *testing.T) {
		ac, _ := newTestClient(t)
		resp, err := ac.RestoreSubscription(context.Background(), StripeService, nil)
		require.Error(t, err)
		assert.Nil(t, resp)
	})
}

func TestPlans(t *testing.T) {
	ac, _ := newTestClient(t)
	resp, err := ac.SubscriptionPlans(context.Background(), "store")
	require.NoError(t, err)
	assert.NotEmpty(t, resp)
}
