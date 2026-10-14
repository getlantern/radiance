package account

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
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

	t.Run("google switches to the purchasing account", func(t *testing.T) {
		ac, _ := newTestClient(t)
		ac.setData(&UserData{
			LegacyID:       1,
			LegacyToken:    "old-token",
			Token:          "old-jwt",
			LegacyUserData: &protos.LoginResponse_UserData{UserId: 1, Email: "old@example.com", UserLevel: "pro"},
		}, nil)
		setOAuthFlags(t)
		changes := make(chan struct{}, 4)
		sub := events.Subscribe(func(UserChangeEvent) { changes <- struct{}{} })
		defer sub.Unsubscribe()

		resp, err := ac.RestoreSubscription(context.Background(), GoogleService, map[string]string{"purchaseToken": "t"})
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, "active", resp.Status)

		assert.Equal(t, int64(1234), settings.GetInt64(settings.UserIDKey))
		assert.Equal(t, "token", settings.GetString(settings.TokenKey))
		assert.Empty(t, settings.GetString(settings.JwtTokenKey))
		assert.Empty(t, settings.GetString(settings.EmailKey))
		assert.False(t, settings.IsPro())
		assert.False(t, settings.GetBool(settings.OAuthLoginKey))
		assert.Empty(t, settings.GetString(settings.OAuthProviderKey))
		select {
		case <-changes:
		case <-time.After(3 * time.Second):
			t.Fatal("account switch not reported")
		}
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

func TestVerifySwitchesToPurchaser(t *testing.T) {
	a, _ := newTestClient(t)
	a.setData(&UserData{LegacyID: 1, LegacyToken: "old-token"}, nil)
	require.NoError(t, a.storeSalt([]byte("previous-salt")))
	useProServer(t, a, serveJSON(VerifySubscriptionResponse{Status: "active", ActualUserID: 99, ActualUserToken: "purchaser"}))

	_, err := a.VerifySubscription(context.Background(), AppleService, map[string]string{"receipt": "r"})
	require.NoError(t, err)

	assert.Equal(t, int64(99), settings.GetInt64(settings.UserIDKey))
	assert.Equal(t, "purchaser", settings.GetString(settings.TokenKey))
	assert.Nil(t, a.getSaltCached(), "the previous account's salt must not carry over")
	saved, err := readSalt(a.saltPath)
	require.NoError(t, err)
	assert.Nil(t, saved)
}

func TestPurchaseSwitchNotSavedKeepsAccount(t *testing.T) {
	a, _ := newTestClient(t)
	require.NoError(t, a.setData(&UserData{LegacyID: 1, LegacyToken: "old-token"}, nil))
	require.NoError(t, a.storeSalt([]byte("previous-salt")))
	useProServer(t, a, serveJSON(VerifySubscriptionResponse{Status: "active", ActualUserID: 99, ActualUserToken: "purchaser"}))
	defer failSettingsSaves(t)()

	_, err := a.VerifySubscription(context.Background(), AppleService, map[string]string{"receipt": "r"})

	assert.NoError(t, err, "the purchase itself succeeded")
	assert.Equal(t, int64(1), settings.GetInt64(settings.UserIDKey))
	assert.Equal(t, []byte("previous-salt"), a.getSaltCached())
}

func TestLatePurchaseResponseDoesNotSwitch(t *testing.T) {
	a, _ := newTestClient(t)
	a.setData(&UserData{LegacyID: 1, LegacyToken: "a"}, nil)
	handler, requested, release := heldJSON(VerifySubscriptionResponse{Status: "active", ActualUserID: 99, ActualUserToken: "purchaser"})
	useProServer(t, a, handler)

	errc := make(chan error, 1)
	go func() {
		_, err := a.VerifySubscription(context.Background(), AppleService, map[string]string{"receipt": "r"})
		errc <- err
	}()
	<-requested
	a.setData(&UserData{LegacyID: 2, LegacyToken: "b"}, nil)
	close(release)

	assert.NoError(t, <-errc, "the purchase itself succeeded")
	assert.Equal(t, int64(2), settings.GetInt64(settings.UserIDKey))
	assert.Equal(t, "b", settings.GetString(settings.TokenKey))
}

func TestSameAccountPurchaseKeepsSaltAndOAuth(t *testing.T) {
	a, _ := newTestClient(t)
	a.setData(&UserData{LegacyID: 99, LegacyToken: "old-token"}, nil)
	require.NoError(t, a.storeSalt([]byte("my-salt")))
	setOAuthFlags(t)
	useProServer(t, a, serveJSON(VerifySubscriptionResponse{Status: "active", ActualUserID: 99, ActualUserToken: "new-token"}))

	_, err := a.VerifySubscription(context.Background(), AppleService, map[string]string{"receipt": "r"})
	require.NoError(t, err)

	assert.Equal(t, "new-token", settings.GetString(settings.TokenKey))
	assert.Equal(t, []byte("my-salt"), a.getSaltCached())
	saved, err := readSalt(a.saltPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("my-salt"), saved)
	assert.True(t, settings.GetBool(settings.OAuthLoginKey))
	assert.Equal(t, "google", settings.GetString(settings.OAuthProviderKey))
}

func TestVerifySucceedsWithAnUnexpectedBody(t *testing.T) {
	a, _ := newTestClient(t)
	useProServer(t, a, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	_, err := a.VerifySubscription(context.Background(), AppleService, map[string]string{"receipt": "r"})

	assert.NoError(t, err)
}
