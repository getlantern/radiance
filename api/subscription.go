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
	"github.com/getlantern/radiance/traces"
	"go.opentelemetry.io/otel"
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
	PublishableKey string `json:"publishableKey"`
}

// SubscriptionPlans retrieves available subscription plans for a given channel.
func (ac *APIClient) SubscriptionPlans(ctx context.Context, channel string) (*SubscriptionPlans, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "subscription_plans")
	defer span.End()

	var resp SubscriptionPlans
	params := map[string]string{
		"locale":              ac.userInfo.Locale(),
		"distributionChannel": channel,
	}
	proWC := ac.proWebClient()
	req := proWC.NewRequest(params, nil, nil)
	err := proWC.Get(ctx, "/plans-v5", req, &resp)
	if err != nil {
		slog.Error("retrieving plans", "error", err)
		return nil, traces.RecordError(ctx, err)
	}
	if resp.BaseResponse != nil && resp.Error != "" {
		err = fmt.Errorf("received bad response: %s", resp.Error)
		slog.Error("retrieving plans", "error", err)
		return nil, traces.RecordError(ctx, err)
	}
	return &resp, nil
}

// NewStripeSubscription creates a new Stripe subscription for the given email and plan ID.
func (ac *APIClient) NewStripeSubscription(ctx context.Context, email, planID string) (*SubscriptionResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "new_stripe_subscription")
	defer span.End()

	data := map[string]string{
		"email":  email,
		"planId": planID,
	}
	proWC := ac.proWebClient()
	req := proWC.NewRequest(nil, nil, data)
	var resp SubscriptionResponse
	err := proWC.Post(ctx, "/stripe-subscription", req, &resp)
	if err != nil {
		slog.Error("creating new subscription", "error", err)
		return nil, traces.RecordError(ctx, fmt.Errorf("creating new subscription: %w", err))
	}
	return &resp, nil
}

// VerifySubscription verifies a subscription for a given service (Google or Apple). data
// should contain the information required by service to verify the subscription, such as the
// purchase token for Google Play or the receipt for Apple. The status and subscription ID are returned
// along with any error that occurred during the verification process.
func (ac *APIClient) VerifySubscription(ctx context.Context, service SubscriptionService, data map[string]string) (status, subID string, err error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "verify_subscription")
	defer span.End()

	var path string
	switch service {
	case GoogleService:
		path = "/purchase-googleplay-subscription"
		data["idempotencyKey"] = strconv.FormatInt(time.Now().UnixNano(), 10)
	case AppleService:
		path = "/purchase-apple-subscription"
	default:
		return "", "", traces.RecordError(ctx, fmt.Errorf("unsupported service: %s", service))
	}

	proWC := ac.proWebClient()
	req := proWC.NewRequest(nil, nil, data)
	type response struct {
		Status         string
		SubscriptionId string
	}
	var resp response
	err = proWC.Post(ctx, path, req, &resp)
	if err != nil {
		slog.Error("verifying subscription", "error", err)
		return "", "", traces.RecordError(ctx, fmt.Errorf("verifying subscription: %w", err))
	}
	return resp.Status, resp.SubscriptionId, nil
}

// StripeBillingPortalUrl generates the Stripe billing portal URL for the given user ID.
func (ac *APIClient) StripeBillingPortalUrl(ctx context.Context) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "stripe_billing_portal_url")
	defer span.End()
	portalURL, err := url.Parse(fmt.Sprintf("%s/%s", proServerURL, "stripe-billing-portal"))
	if err != nil {
		slog.Error("parsing portal URL", "error", err)
		return "", traces.RecordError(ctx, fmt.Errorf("parsing portal URL: %w", err))
	}
	query := portalURL.Query()
	query.Set("referer", "https://lantern.io/")
	query.Set("userId", strconv.FormatInt(int64(ac.userInfo.LegacyID()), 10))
	query.Set("proToken", ac.userInfo.LegacyToken())
	portalURL.RawQuery = query.Encode()
	return portalURL.String(), nil
}

// SubscriptionPaymentRedirectURL generates a redirect URL for subscription payment.
func (ac *APIClient) SubscriptionPaymentRedirectURL(ctx context.Context, data PaymentRedirectData) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "subscription_payment_redirect_url")
	defer span.End()

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
	proWC := ac.proWebClient()
	req := proWC.NewRequest(params, headers, nil)
	err := proWC.Get(ctx, "/subscription-payment-redirect", req, &resp)
	if err != nil {
		slog.Error("subscription payment redirect", "error", err)
		return "", traces.RecordError(ctx, fmt.Errorf("subscription payment redirect: %w", err))
	}
	return resp.Redirect, traces.RecordError(ctx, err)
}

// PaymentRedirect is used to get the payment redirect URL with PaymentRedirectData
// this is used in desktop app and android app
func (ac *APIClient) PaymentRedirect(ctx context.Context, data PaymentRedirectData) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "payment_redirect")
	defer span.End()

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
	proWC := ac.proWebClient()
	req := proWC.NewRequest(mapping, headers, nil)
	err := proWC.Get(ctx, "/payment-redirect", req, &resp)
	if err != nil {
		slog.Error("subscription payment redirect", "error", err)
		return "", traces.RecordError(ctx, fmt.Errorf("subscription payment redirect: %w", err))
	}
	return resp.Redirect, traces.RecordError(ctx, err)
}

type PurchaseResponse struct {
	*protos.BaseResponse `json:",inline"`
	PaymentStatus        string      `json:"paymentStatus"`
	Plan                 protos.Plan `json:"plan"`
	Status               string      `json:"status"`
}

// ActivationCode is used to purchase a subscription using a reseller code.
func (ac *APIClient) ActivationCode(ctx context.Context, email, resellerCode string) (*PurchaseResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "activation_code")
	defer span.End()

	data := map[string]interface{}{
		"idempotencyKey": strconv.FormatInt(time.Now().UnixNano(), 10),
		"provider":       "reseller-code",
		"email":          email,
		"deviceName":     ac.userInfo.DeviceID(),
		"resellerCode":   resellerCode,
	}
	var resp PurchaseResponse
	proWC := ac.proWebClient()
	req := proWC.NewRequest(nil, nil, data)
	err := proWC.Post(ctx, "/purchase", req, &resp)
	if err != nil {
		slog.Error("retrieving subscription status", "error", err)
		return nil, traces.RecordError(ctx, fmt.Errorf("retrieving subscription status: %w", err))
	}
	if resp.BaseResponse != nil && resp.Error != "" {
		slog.Error("retrieving subscription status", "error", err)
		return nil, traces.RecordError(ctx, fmt.Errorf("received bad response: %s", resp.Error))
	}
	return &resp, nil
}
