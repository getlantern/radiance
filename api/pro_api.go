package api

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

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
	GoogleSubscription(ctx context.Context, purchaseToken, planId string) (*protos.AcknowledgmentResponse, error)
	AppleSubscription(ctx context.Context, purchaseToken, planId string) (*protos.AcknowledgmentResponse, error)
	// User methods
	CreateUser(ctx context.Context) (*protos.UserDataResponse, error)
	UserData(ctx context.Context) (*protos.UserDataResponse, error)
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

// UserData will request the user data and return the response.
func (c *proClient) UserData(ctx context.Context) (*protos.UserDataResponse, error) {
	var resp *protos.UserDataResponse
	err := c.Get(ctx, "/user-data", nil, &resp)
	if err != nil {
		err = fmt.Errorf("seding user data request: %v", err)
		slog.Error("", "err", err)
		return nil, err
	}
	if resp.BaseResponse != nil && resp.BaseResponse.Error != "" {
		err = fmt.Errorf("recevied bad response: %s", resp.BaseResponse.Error)
		slog.Error("", "err", err)
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
		err = fmt.Errorf("writing user data: %v", err)
		slog.Error("", "err", err)
		return nil, err
	}
	return resp, nil

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

// StripeSubscription is used to create a new subscription with SubscriptionRequest
// and return the subscription data
// this is used only in android app
func (c *proClient) StripeSubscription(ctx context.Context, data *protos.SubscriptionRequest) (*protos.SubscriptionResponse, error) {
	slog.Debug("StripeSubscription api", "data", data)
	var resp *protos.SubscriptionResponse
	mapping := map[string]any{
		"email":  data.Email,
		"planId": data.PlanId,
	}
	err := c.Post(ctx, "/stripe-subscription", mapping, &resp)
	if err != nil {
		slog.Error("Error in stripe subscription: %v", "err", err)
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
	err := c.Get(ctx, "/plans-v5", params, &resp)
	if err != nil {
		slog.Error("Error in Plans: %v", "err", err)
		return nil, err
	}
	return resp, nil
}

// GoogleSubscription is used to create a acnknowledgement for google subscription
// this is used only in android app
func (c *proClient) GoogleSubscription(ctx context.Context, purchaseToken, planId string) (*protos.AcknowledgmentResponse, error) {
	var resp *protos.AcknowledgmentResponse
	mapping := map[string]any{
		"purchaseToken":  purchaseToken,
		"planId":         planId,
		"idempotencyKey": strconv.FormatInt(time.Now().UnixNano(), 10),
	}
	slog.Debug("GoogleSubscription api", "mapping", mapping)
	err := c.Post(ctx, "/purchase-googleplay-subscription", mapping, &resp)
	if err != nil {
		slog.Error("Error in google play: %v", "err", err)
		return nil, err
	}
	return resp, nil
}

// AppleSubscription is used to create a acnknowledgement for apple subscription
// this is used only in ios app
func (c *proClient) AppleSubscription(ctx context.Context, purchaseToken, planId string) (*protos.AcknowledgmentResponse, error) {
	var resp *protos.AcknowledgmentResponse
	mapping := map[string]any{
		"receipt": purchaseToken,
		"planId":  planId,
	}
	err := c.Post(ctx, "/purchase-apple-subscription", mapping, &resp)
	if err != nil {
		slog.Error("error in apple: %v", "err", err)
		return nil, err
	}
	return resp, nil
}
