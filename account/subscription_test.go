package account

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
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
	assert.Equal(t, common.GetVersion(), ts.paymentRedirectAppVersion)
	assert.Equal(t, common.GetVersion(), ts.paymentRedirectLibraryVersion)
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

func TestPaymentRedirectForwardsCouponCode(t *testing.T) {
	ac, ts := newTestClient(t)
	data := PaymentRedirectData{
		Provider:   "stripe",
		Plan:       "pro",
		DeviceName: "test-device",
		CouponCode: "SAVE20",
	}

	_, err := ac.PaymentRedirect(context.Background(), data)
	require.NoError(t, err)
	assert.True(t, ts.paymentRedirectHasCouponCode)
	assert.Equal(t, data.CouponCode, ts.paymentRedirectCouponCode)

	_, err = ac.SubscriptionPaymentRedirectURL(context.Background(), data)
	require.NoError(t, err)
	assert.True(t, ts.subscriptionPaymentRedirectHasCouponCode)
	assert.Equal(t, data.CouponCode, ts.subscriptionPaymentRedirectCouponCode)
}

func TestPaymentRedirectOmitsEmptyCouponCode(t *testing.T) {
	ac, ts := newTestClient(t)
	data := PaymentRedirectData{
		Provider:   "stripe",
		Plan:       "pro",
		DeviceName: "test-device",
	}

	_, err := ac.PaymentRedirect(context.Background(), data)
	require.NoError(t, err)
	assert.False(t, ts.paymentRedirectHasCouponCode)

	_, err = ac.SubscriptionPaymentRedirectURL(context.Background(), data)
	require.NoError(t, err)
	assert.False(t, ts.subscriptionPaymentRedirectHasCouponCode)
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
	require.NoError(t, ac.storeIdentity(123, "token"))
	resp, err := ac.SubscriptionPlans(context.Background(), "store")
	require.NoError(t, err)
	assert.NotEmpty(t, resp)
}

func TestPlansWaitsForNewUser(t *testing.T) {
	ac, _ := newTestClient(t)
	require.NoError(t, settings.Clear(settings.UserIDKey)) // InitSettings may migrate a local install's user
	type result struct {
		resp string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := ac.SubscriptionPlans(context.Background(), "store")
		done <- result{resp, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("plans returned before a user existed: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, ac.storeIdentity(123, "token"))
	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.NotEmpty(t, r.resp)
	case <-time.After(5 * time.Second):
		t.Fatal("plans did not proceed after the user was created")
	}
}

func TestPlansWithoutUserStopsWithContext(t *testing.T) {
	ac, _ := newTestClient(t)
	require.NoError(t, settings.Clear(settings.UserIDKey)) // InitSettings may migrate a local install's user
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := ac.SubscriptionPlans(ctx, "store")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestNewStripeSubscriptionForwardsCouponCode(t *testing.T) {
	ac, ts := newTestClient(t)
	resp, err := ac.NewStripeSubscription(context.Background(), "test@getlantern.org", "1y-usd-10", "SAVE20")
	require.NoError(t, err)
	assert.NotEmpty(t, resp)
	assert.True(t, ts.stripeSubscriptionHasCouponCode)
	assert.Equal(t, "SAVE20", ts.stripeSubscriptionCouponCode)
}

func TestNewStripeSubscriptionOmitsEmptyCouponCode(t *testing.T) {
	ac, ts := newTestClient(t)
	resp, err := ac.NewStripeSubscription(context.Background(), "test@getlantern.org", "1y-usd-10", "")
	require.NoError(t, err)
	assert.NotEmpty(t, resp)
	assert.False(t, ts.stripeSubscriptionHasCouponCode)
}
