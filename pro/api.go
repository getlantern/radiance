package pro

import (
	"context"
	"log"
	"log/slog"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/user/protos"
)

type proClient struct {
	common.WebClient
	common.UserConfig
}

type ProClient interface {
	//Payment methods
	SubscriptionPaymentRedirect(ctx context.Context, data *protos.SubscriptionPaymentRedirectRequest) (*protos.SubscriptionPaymentRedirectResponse, error)
	StripeSubscription(ctx context.Context, data *protos.SubscriptionRequest) (*protos.SubscriptionResponse, error)
	UserCreate(ctx context.Context) (*protos.UserDataResponse, error)
}

func (c *proClient) SubscriptionPaymentRedirect(ctx context.Context, data *protos.SubscriptionPaymentRedirectRequest) (*protos.SubscriptionPaymentRedirectResponse, error) {
	var resp *protos.SubscriptionPaymentRedirectResponse
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

func (c *proClient) UserCreate(ctx context.Context) (*protos.UserDataResponse, error) {
	var resp *protos.UserDataResponse
	err := c.Post(ctx, "/user-create", nil, &resp)
	if err != nil {
		log.Fatalf("Error in UserCreate: %v", err)
		return nil, err
	}
	login := &protos.LoginResponse{
		LegacyID:       resp.LoginResponse_UserData.UserId,
		LegacyToken:    resp.LoginResponse_UserData.Token,
		LegacyUserData: resp.LoginResponse_UserData,
	}
	/// Write user data to file
	err = c.UserConfig.Save(login)
	if err != nil {
		log.Fatalf("Error writing user data: %v", err)
		return nil, err
	}
	return resp, nil

}

func (c *proClient) StripeSubscription(ctx context.Context, data *protos.SubscriptionRequest) (*protos.SubscriptionResponse, error) {
	slog.Debug("StripeSubscription api", "data", data)
	var resp *protos.SubscriptionResponse
	mapping := map[string]any{
		"email":   data.Email,
		"name":    data.Name,
		"priceId": data.PriceId,
	}
	err := c.Post(ctx, "/stripe-subscription", mapping, &resp)
	if err != nil {
		log.Fatalf("Error in UserCreate: %v", err)
		return nil, err
	}
	return resp, nil

}
