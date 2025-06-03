package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"time"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/backend"
)

const proServerURL = "https://api.getiantem.org"

type (
	SubscriptionService string
	SubscriptionType    string
)

const (
	StripeService SubscriptionService = "stripe"
	AppleService  SubscriptionService = "apple"
	GoogleService SubscriptionService = "google"

	SubscriptionTypeOneTime      SubscriptionType = "one_time"
	SubscriptionTypeSubscription SubscriptionType = "subscription"
)

// PaymentRedirectData contains the data required to generate a payment redirect URL.
type PaymentRedirectData struct {
	Plan        string           `json:"plan" validate:"required"`
	Provider    string           `json:"provider" validate:"required"`
	Email       string           `json:"email"`
	DeviceName  string           `json:"deviceName" validate:"required" errorId:"device-name"`
	BillingType SubscriptionType `json:"billingType"`
}

// SubscriptionPlans contains information about available subscription plans and payment providers.
type SubscriptionPlans struct {
	*protos.BaseResponse `json:",inline"`
	Providers            map[string][]*protos.PaymentMethod `json:"providers"`
	Plans                []*protos.Plan                     `json:"plans"`
}

// SubscriptionResponse contains information about a created subscription.
type SubscriptionResponse struct {
	CustomerId     string `json:"customerId"`
	SubscriptionId string `json:"subscriptionId"`
	ClientSecret   string `json:"clientSecret"`
}

// SubscriptionPlans retrieves available subscription plans for a given channel.
func (ac *APIClient) SubscriptionPlans(ctx context.Context, channel string) (*SubscriptionPlans, error) {
	var resp SubscriptionPlans
	params := map[string]string{
		"locale":              ac.userInfo.Locale(),
		"distributionChannel": channel,
	}
	req := ac.proWC.NewRequest(params, nil, nil)
	err := ac.proWC.Get(ctx, "/plans-v5", req, &resp)
	if err != nil {
		slog.Error("retrieving plans", "error", err)
		return nil, err
	}
	if resp.BaseResponse != nil && resp.Error != "" {
		err = fmt.Errorf("recievied bad response: %s", resp.Error)
		slog.Error("retrieving plans", "error", err)
		return nil, err
	}
	return &resp, nil
}

// NewStripeSubscription creates a new Stripe subscription for the given email and plan ID.
func (ac *APIClient) NewStripeSubscription(ctx context.Context, email, planID string) (*SubscriptionResponse, error) {
	data := map[string]string{
		"email":  email,
		"planId": planID,
	}
	req := ac.proWC.NewRequest(nil, nil, data)
	var resp SubscriptionResponse
	err := ac.proWC.Post(ctx, "/stripe-subscription", req, &resp)
	if err != nil {
		slog.Error("creating new subscription", "error", err)
		return nil, fmt.Errorf("creating new subscription: %w", err)
	}
	return &resp, nil
}

// VerifySubscription verifies a subscription for a given service (Google or Apple). data
// should contain the information required by service to verify the subscription, such as the
// purchase token for Google Play or the receipt for Apple. The status and subscription ID are returned
// along with any error that occurred during the verification process.
func (ac *APIClient) VerifySubscription(ctx context.Context, service SubscriptionService, data map[string]string) (status, subID string, err error) {
	var path string
	switch service {
	case GoogleService:
		path = "/purchase-googleplay-subscription"
		data["idempotencyKey"] = strconv.FormatInt(time.Now().UnixNano(), 10)
	case AppleService:
		path = "/purchase-apple-subscription"
	default:
		return "", "", fmt.Errorf("unsupported service: %s", service)
	}

	req := ac.proWC.NewRequest(nil, nil, data)
	type response struct {
		Status         string
		SubscriptionId string
	}
	var resp response
	err = ac.proWC.Post(ctx, path, req, &resp)
	if err != nil {
		slog.Error("verifying subscription", "error", err)
		return "", "", fmt.Errorf("verifying subscription: %w", err)
	}
	return resp.Status, resp.SubscriptionId, nil
}

// StripeBillingPortalUrl generates the Stripe billing portal URL for the given user ID.
func (ac *APIClient) StripeBillingPortalUrl() (string, error) {
	portalURL, err := url.Parse(fmt.Sprintf("%s/%s", proServerURL, "stripe-billing-portal"))
	if err != nil {
		return "", fmt.Errorf("failed to parse URL: %w", err)
	}
	query := portalURL.Query()
	query.Set("referer", "https://lantern.io/")
	query.Set("userId", strconv.FormatInt(int64(ac.userInfo.LegacyID()), 10))
	portalURL.RawQuery = query.Encode()

	return portalURL.String(), nil
}

// SubscriptionPaymentRedirectURL generates a redirect URL for subscription payment.
func (ac *APIClient) SubscriptionPaymentRedirectURL(ctx context.Context, data PaymentRedirectData) (string, error) {
	type response struct {
		Redirect string
	}
	var resp response
	headers := map[string]string{
		backend.RefererHeader: "https://lantern.io/",
	}
	params := map[string]string{
		"provider":    data.Provider,
		"plan":        data.Plan,
		"deviceName":  data.DeviceName,
		"email":       data.Email,
		"billingType": string(data.BillingType),
	}
	req := ac.proWC.NewRequest(params, headers, nil)
	err := ac.proWC.Get(ctx, "/subscription-payment-redirect", req, &resp)
	if err != nil {
		slog.Error("subscription payment redirect", "error", err)
		return "", fmt.Errorf("subscription payment redirect: %w", err)
	}
	return resp.Redirect, err
}

// PaymentRedirect is used to get the payment redirect URL with PaymentRedirectData
// this is used in desktop app and android app
func (ac *APIClient) PaymentRedirect(ctx context.Context, data PaymentRedirectData) (string, error) {
	type response struct {
		Redirect string
	}
	var resp response
	headers := map[string]string{
		backend.RefererHeader: "https://lantern.io/",
	}
	mapping := map[string]string{
		"provider":   data.Provider,
		"plan":       data.Plan,
		"deviceName": data.DeviceName,
		"email":      data.Email,
	}
	req := ac.proWC.NewRequest(mapping, headers, nil)
	err := ac.proWC.Get(ctx, "/payment-redirect", req, &resp)
	if err != nil {
		slog.Error("subscription payment redirect", "error", err)
		return "", fmt.Errorf("subscription payment redirect: %w", err)
	}
	return resp.Redirect, err
}
