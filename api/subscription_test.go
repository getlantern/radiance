package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
)

// TODO: update tests to use a mock server instead of the real one

func TestSubscriptionPaymentRedirect(t *testing.T) {
	user := testUser(t.TempDir())
	ac := NewAPIClient(&http.Client{}, user, t.TempDir())
	data := PaymentRedirectData{
		Provider:         "stripe",
		Plan:             "pro",
		DeviceName:       "test-device",
		Email:            "",
		SubscriptionType: SubscriptionType("monthly"),
	}
	url, err := ac.SubscriptionPaymentRedirectURL(context.Background(), data)
	require.NoError(t, err)
	assert.NotEmpty(t, url)
}

func TestNewUser(t *testing.T) {
	user := testUser(t.TempDir())
	ac := NewAPIClient(&http.Client{}, user, t.TempDir())
	resp, err := ac.NewUser(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestVerifySubscription(t *testing.T) {
	user := testUser(t.TempDir())
	ac := NewAPIClient(&http.Client{}, user, t.TempDir())
	email := "test@getlantern.org"
	planID := "1y-usd-10"
	data := map[string]string{
		"email":  email,
		"planID": planID,
	}
	status, subID, err := ac.VerifySubscription(context.Background(), AppleService, data)
	require.NoError(t, err)
	assert.NotEmpty(t, status)
	assert.NotEmpty(t, subID)
}

func TestPlans(t *testing.T) {
	user := testUser(t.TempDir())
	ac := NewAPIClient(&http.Client{}, user, t.TempDir())

	// TODO: remove this when we switch to a mock server
	_, err := ac.NewUser(context.Background())
	require.NoError(t, err)

	resp, err := ac.SubscriptionPlans(context.Background(), "store")
	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.NotNil(t, resp.Plans)
}

func testUser(dataPath string) common.UserInfo {
	return common.NewUserConfig("HFJDFJ-75885F", dataPath, "en-US")
}
