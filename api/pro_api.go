package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
)

// ProClient is the interface for the Pro API
// It contains methods for subscription and user management
type proClient struct {
	common.WebClient
	common.UserInfo
}

type ProClient interface {
	//Payment methods
	SubscriptionPaymentRedirect(ctx context.Context, data *protos.SubscriptionPaymentRedirectRequest) (*protos.SubscriptionPaymentRedirectResponse, error)
	StripeSubscription(ctx context.Context, data *protos.SubscriptionRequest) (*protos.SubscriptionResponse, error)
	Plans(ctx context.Context) (*protos.PlansResponse, error)
	// User methods
	CreateUser(ctx context.Context) (*protos.UserDataResponse, error)
	UserData(ctx context.Context) (*protos.UserDataResponse, error)
}

// SubscriptionPaymentRedirect is used to get the subscription payment redirect URL with SubscriptionPaymentRedirectRequest
// this is is used only in desktop app
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

// CreateUser is used to create a new user
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

// UserData is used to get user data
// this will be also save data to user config
func (c *proClient) UserData(ctx context.Context) (*protos.UserDataResponse, error) {
	var resp *protos.UserDataResponse
	err := c.Get(ctx, "/user-data", nil, &resp)
	if err != nil {
		slog.Error("Error in UserData: %v", "err", err)
		return nil, err
	}
	if resp.BaseResponse != nil && resp.BaseResponse.Error != "" {
		slog.Error("Error in UserData: %v", "err", resp.BaseResponse.Error)
		return nil, fmt.Errorf("error in UserData: %s", resp.BaseResponse.Error)
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

// StripeSubscription is used to create a new subscription with SubscriptionRequest
// and return the subscription data
// this is used only in android app
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

// Plans is used to get the list of plans
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
