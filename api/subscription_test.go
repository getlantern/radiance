package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/common"
	"github.com/getlantern/radiance/api/protos"
	rcommon "github.com/getlantern/radiance/common"
)

func TestSubscriptionPaymentRedirect(t *testing.T) {
	ac := mockAPIClient(t)
	data := PaymentRedirectData{
		Provider:    "stripe",
		Plan:        "pro",
		DeviceName:  "test-device",
		Email:       "",
		BillingType: SubscriptionTypeOneTime,
	}
	url, err := ac.SubscriptionPaymentRedirectURL(context.Background(), data)
	require.NoError(t, err)
	assert.NotEmpty(t, url)
}
func TestPaymentRedirect(t *testing.T) {
	ac := mockAPIClient(t)
	data := PaymentRedirectData{
		Provider:   "stripe",
		Plan:       "pro",
		DeviceName: "test-device",
		Email:      "",
	}
	url, err := ac.PaymentRedirect(context.Background(), data)
	require.NoError(t, err)
	assert.NotEmpty(t, url)
}

func TestNewUser(t *testing.T) {
	ac := mockAPIClient(t)
	resp, err := ac.NewUser(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestVerifySubscription(t *testing.T) {
	ac := mockAPIClient(t)
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
	ac := mockAPIClient(t)
	resp, err := ac.SubscriptionPlans(context.Background(), "store")
	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.NotNil(t, resp.Plans)
}

func userInfo(dataPath string) rcommon.UserInfo {
	return rcommon.NewUserConfig("HFJDFJ-75885F", dataPath, "en-US")
}

type MockAPIClient struct {
	*APIClient
}

func mockAPIClient(t *testing.T) *MockAPIClient {
	return &MockAPIClient{
		APIClient: &APIClient{
			saltPath: filepath.Join(t.TempDir(), saltFileName),
			userInfo: newUserInfo(&common.UserData{
				Email: "test@example.com",
			}),
			deviceID: "deviceId",
			salt:     []byte{1, 2, 3, 4, 5},
		},
	}
}

func (m *MockAPIClient) VerifySubscription(ctx context.Context, service SubscriptionService, data map[string]string) (status, subID string, err error) {
	return "active", "sub_1234567890", nil
}

func (m *MockAPIClient) SubscriptionPlans(ctx context.Context, channel string) (*SubscriptionPlans, error) {
	resp := &SubscriptionPlans{
		BaseResponse: &protos.BaseResponse{},
		Plans: []*protos.Plan{
			{Id: "1y-usd-10", Description: "Pro Plan", Price: map[string]int64{}},
		},
	}
	return resp, nil
}
func (m *MockAPIClient) SubscriptionPaymentRedirectURL(ctx context.Context, data PaymentRedirectData) (string, error) {
	return "https://example.com/redirect", nil
}

func (m *MockAPIClient) PaymentRedirect(ctx context.Context, data PaymentRedirectData) (string, error) {
	return "https://example.com/redirect", nil
}
func (m *MockAPIClient) NewUser(ctx context.Context) (*protos.LoginResponse, error) {
	return &protos.LoginResponse{}, nil
}
