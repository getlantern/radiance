package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/zeebo/assert"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
)

func TestSubscriptionPaymentRedirect(t *testing.T) {
	proServer := NewPro(&http.Client{}, commonConfig())
	body := &protos.SubscriptionPaymentRedirectRequest{
		Provider:         "stripe",
		Plan:             "pro",
		DeviceName:       "test-device",
		Email:            "",
		SubscriptionType: protos.SubscriptionType("monthly"),
	}
	resp, error := proServer.SubscriptionPaymentRedirect(context.Background(), body)
	assert.NoError(t, error)
	assert.NotNil(t, resp.Redirect)
}

func TestCreateUser(t *testing.T) {
	proServer := NewPro(&http.Client{}, commonConfig())
	resp, error := proServer.UserCreate(context.Background())
	assert.NoError(t, error)
	assert.NotNil(t, resp)
}

func TestStripeSubscription(t *testing.T) {
	proServer := NewPro(&http.Client{}, commonConfig())
	body := &protos.SubscriptionRequest{
		Email:   "test@getlantern.org",
		Name:    "Test User",
		PriceId: "price_1RCg464XJ6zbDKY5T6kqbMC6",
	}
	resp, error := proServer.StripeSubscription(context.Background(), body)
	assert.NoError(t, error)
	assert.NotNil(t, resp)
}

func TestPlans(t *testing.T) {
	proServer := NewPro(&http.Client{}, commonConfig())
	resp, error := proServer.Plans(context.Background())
	assert.NoError(t, error)
	assert.NotNil(t, resp)
	assert.NotNil(t, resp.Plans)
}

func commonConfig() common.UserInfo {
	return common.NewUserConfig("HFJDFJ-75885F", "", "en-US")
}
