package api

import (
	"context"
	"log/slog"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
)

type proClient struct {
	common.WebClient
	common.UserInfo
}

type ProClient interface {
	//Payment methods
	SubscriptionPaymentRedirect(ctx context.Context, data *protos.SubscriptionPaymentRedirectRequest) (*protos.SubscriptionPaymentRedirectResponse, error)
	StripeSubscription(ctx context.Context, data *protos.SubscriptionRequest) (*protos.SubscriptionResponse, error)
	CreateUser(ctx context.Context) (*protos.UserDataResponse, error)
	Plans(ctx context.Context) (*protos.PlansResponse, error)
}

//
func (c *proClient) SubscriptionPaymentRedirect(ctx context.Context, data *protos.SubscriptionPaymentRedirectRequest) (*protos.SubscriptionPaymentRedirectResponse, error) {
	var resp *protos.SubscriptionPaymentRedirectResponse

	err := c.Get(ctx, "/subscription-payment-redirect", map[string]any{
		"provider":         data.Provider,
		"plan":             data.Plan,
		"deviceName":       data.DeviceName,
		"email":            data.Email,
		"subscriptionType": data.SubscriptionType,
	}, &resp)
	if err != nil {
		slog.Error("Error in SubscriptionPaymentRedirect: %v", "err", err)
		return nil, err
	}
	slog.Error("SubscriptionPaymentRedirect response", "resp", resp)
	return resp, nil
}

func (c *proClient) CreateUser(ctx context.Context) (*protos.UserDataResponse, error) {
	var resp *protos.UserDataResponse
	err := c.Post(ctx, "/user-create", nil, &resp)
	if err != nil {
		slog.Error("Error in UserCreate: %v", "err", err)
		return nil, err
	}
	login := &protos.LoginResponse{
		LegacyID:       resp.LoginResponse_UserData.UserId,
		LegacyToken:    resp.LoginResponse_UserData.Token,
		LegacyUserData: resp.LoginResponse_UserData,
	}
	/// Write user data to file
	err = c.UserInfo.Save(login)
	if err != nil {
		slog.Error("Error writing user data: %v", "err", err)
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
		slog.Error("Error in UserCreate: %v", "err", err)
		return nil, err
	}
	return resp, nil

}
func (c *proClient) Plans(ctx context.Context) (*protos.PlansResponse, error) {
	var resp *protos.PlansResponse
	params := map[string]interface{}{
		"locale": c.UserInfo.Locale(),
	}
	err := c.Get(ctx, "/plans-v4", params, &resp)
	if err != nil {
		slog.Error("Error in Plans: %v", "err", err)
		return nil, err
	}
	return resp, nil
}
