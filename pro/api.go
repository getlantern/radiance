package pro

import (
	"context"
	"log"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/user"
)

type proClient struct {
	common.WebClient
}

type ProClient interface {
	//Payment methods
	SubscriptionPaymentRedirect(ctx context.Context, data *SubscriptionPaymentRedirectRequest) (*SubscriptionPaymentRedirectResponse, error)
	UserCreate(ctx context.Context) (*user.UserResponse, error)
}

func (c *proClient) SubscriptionPaymentRedirect(ctx context.Context, data *SubscriptionPaymentRedirectRequest) (*SubscriptionPaymentRedirectResponse, error) {
	var resp *SubscriptionPaymentRedirectResponse
	err := c.Get(ctx, "subscription-payment-redirect", map[string]any{
		"provider":         "stripe",
		"plan":             "1y-usd",
		"deviceName":       "test",
		"email":            "jigar+test2@getlantern.org",
		"subscriptionType": "monthly",
	}, &resp)
	if err != nil {
		log.Fatalf("Error in SubscriptionPaymentRedirect: %v", err)
		return nil, err
	}
	log.Printf("SubscriptionPaymentRedirect response: %v", resp)
	return resp, nil
}

func (c *proClient) UserCreate(ctx context.Context) (*user.UserResponse, error) {
	var resp *user.UserResponse
	err := c.Post(ctx, "/user-create", nil, &resp)
	if err != nil {
		log.Fatalf("Error in UserCreate: %v", err)
		return nil, err
	}
	log.Printf("UserCreate response: %v", resp)
	return resp, nil

}
