package pro

import (
	"context"
	"log"

	"github.com/getlantern/radiance/common"
)

type proClient struct {
	common.WebClient
}

type ProClient interface {
	//Payment methods
	SubscriptionPaymentRedirect(ctx context.Context, data *SubscriptionPaymentRedirectRequest) (any, error)
}

func (c *proClient) SubscriptionPaymentRedirect(ctx context.Context, data *SubscriptionPaymentRedirectRequest) (any, error) {
	var resp SubscriptionPaymentRedirectResponse
	err := c.Get(ctx, "/subscription/payment/redirect", map[string]any{
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
