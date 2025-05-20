package api

import (
	"context"
	"encoding/json"
	"fmt"
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
		Email:  "test@getlantern.org",
		PlanId: "1y-usd",
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

func TestGoogleSubscription(t *testing.T) {
	var resp = "{\"status\":\"ok\",\"subscriptionId\":\"pefnihpbnllffpggfoldejgf.AO-J1OxJq7zbGLPRXDWsglwAIaQZCkrIL3XQK7iMJsl_2aR6OQ5tDGgHbyYwEjUkxEbiTM-KOnUTELLC2t1WKmdKAikBx_SA_jhPP5zblcPfsHSYb-ZQDIM\"}"
	var ack protos.AcknowledgmentResponse
	if err := json.Unmarshal([]byte(resp), &ack); err != nil {
		fmt.Println("Error unmarshalling JSON:", err)
		return
	}
	fmt.Println("Status:", ack.Status)

}

func commonConfig() common.UserInfo {
	return common.NewUserConfig("HFJDFJ-75885F", "", "en-US")
}
