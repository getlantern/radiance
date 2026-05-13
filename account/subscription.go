package account

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/traces"
)

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
	Plan           string           `json:"plan" validate:"required"`
	Provider       string           `json:"provider" validate:"required"`
	Email          string           `json:"email"`
	DeviceName     string           `json:"deviceName" validate:"required" errorId:"device-name"`
	BillingType    SubscriptionType `json:"billingType"`
	IdempotencyKey string           `json:"idempotencyKey"`
}

type SubscriptionPlans struct {
	*protos.BaseResponse `json:",inline"`
	Providers            map[string][]*protos.PaymentMethod `json:"providers"`
	Plans                []*protos.Plan                     `json:"plans"`
}

type SubscriptionResponse struct {
	CustomerID     string `json:"customerId"`
	SubscriptionID string `json:"subscriptionId"`
	ClientSecret   string `json:"clientSecret"`
	PendingSecret  string `json:"pending_secret"`
	PublishableKey string `json:"publishableKey"`
}

// SubscriptionPlans retrieves available subscription plans for a given channel.
func (a *Client) SubscriptionPlans(ctx context.Context, channel string) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "subscription_plans")
	defer span.End()

	params := map[string]string{
		"locale":              settings.GetString(settings.LocaleKey),
		"distributionChannel": channel,
	}
	resp, err := a.sendProRequest(ctx, "GET", "/plans-v5", params, nil, nil)
	if err != nil {
		slog.Error("retrieving plans", "error", err)
		return "", traces.RecordError(ctx, err)
	}
	var plans SubscriptionPlans
	if err := json.Unmarshal(resp, &plans); err != nil {
		return "", traces.RecordError(ctx, fmt.Errorf("unmarshaling plans response: %w", err))
	}
	if plans.BaseResponse != nil && plans.Error != "" {
		err = fmt.Errorf("received bad response: %s", plans.Error)
		slog.Error("retrieving plans", "error", err)
		return "", traces.RecordError(ctx, err)
	}
	return string(resp), nil
}

// NewStripeSubscription creates a new Stripe subscription for the given email and plan ID.
func (a *Client) NewStripeSubscription(ctx context.Context, email, planID string) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "new_stripe_subscription")
	defer span.End()

	data := map[string]string{
		"email":  email,
		"planId": planID,
	}
	resp, err := a.sendProRequest(ctx, "POST", "/stripe-subscription", nil, nil, data)
	if err != nil {
		return "", traces.RecordError(ctx, fmt.Errorf("creating stripe subscription: %w", err))
	}
	return string(resp), nil
}

type VerifySubscriptionResponse struct {
	Status          string `json:"status"`
	SubscriptionID  string `json:"subscriptionId"`
	ActualUserID    int64  `json:"actualUserId,omitempty"`
	ActualUserToken string `json:"actualUserToken,omitempty"`
}

// VerifySubscription verifies a subscription for a given service (Google or Apple). data
// should contain the information required by service to verify the subscription, such as the
// purchase token for Google Play or the receipt for Apple. The status and subscription ID are returned
// along with any error that occurred during the verification process.
func (a *Client) VerifySubscription(ctx context.Context, service SubscriptionService, data map[string]string) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "verify_subscription")
	defer span.End()

	var path string
	switch service {
	case GoogleService:
		path = "/purchase-googleplay-subscription"
		data["idempotencyKey"] = strconv.FormatInt(time.Now().UnixNano(), 10)
	case AppleService:
		path = "/purchase-apple-subscription-v2"
	default:
		return "", traces.RecordError(ctx, fmt.Errorf("unsupported service: %s", service))
	}

	resp, err := a.sendProRequest(ctx, "POST", path, nil, nil, data)
	if err != nil {
		slog.Error("verifying subscription", "error", err)
		return "", traces.RecordError(ctx, fmt.Errorf("verifying subscription: %w", err))
	}
	return string(resp), nil

}

type RestoreSubscriptionResponse struct {
	Status          string                         `json:"status"`
	ActualUserID    int64                          `json:"actualUserId,omitempty"`
	ActualUserToken string                         `json:"actualUserToken,omitempty"`
	Devices         []*protos.LoginResponse_Device `json:"devices,omitempty"`
}

// RestoreSubscription restores a previously purchased subscription for the given service.
// data should contain the fields required by the backend for the chosen service (e.g.
// "purchaseToken" for Google, "receipt" for Apple); callers are responsible for populating
// it. Services other than [GoogleService] and [AppleService] are rejected.
func (a *Client) RestoreSubscription(ctx context.Context, service SubscriptionService, data map[string]string) (*RestoreSubscriptionResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "restore_subscription")
	defer span.End()

	var path string
	switch service {
	case GoogleService:
		path = "/restore-googleplay-subscription"
	case AppleService:
		path = "/restore-apple-subscription"
	default:
		return nil, traces.RecordError(ctx, fmt.Errorf("unsupported service: %s", service))
	}

	resp, err := a.sendProRequest(ctx, "POST", path, nil, nil, data)
	if err != nil {
		slog.Error("restoring subscription", "error", err)
		return nil, traces.RecordError(ctx, fmt.Errorf("restoring subscription: %w", err))
	}
	var result RestoreSubscriptionResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("unmarshaling restore subscription response: %w", err))
	}
	return &result, nil
}

// StripeBillingPortalURL generates the Stripe billing portal URL for the given user ID.
// baseURL = common.GetProServerURL
func (a *Client) StripeBillingPortalURL(ctx context.Context, baseURL, userID, proToken string) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "stripe_billing_portal_url")
	defer span.End()
	portalURL, err := url.Parse(baseURL + "/stripe-billing-portal")
	if err != nil {
		slog.Error("parsing portal URL", "error", err)
		return "", traces.RecordError(ctx, fmt.Errorf("parsing portal URL: %w", err))
	}
	query := portalURL.Query()
	query.Set("referer", "https://lantern.io/")
	query.Set("userId", userID)
	query.Set("proToken", proToken)
	portalURL.RawQuery = query.Encode()
	return portalURL.String(), nil
}

type redirect struct {
	Redirect string `json:"redirect"`
}

func (a *Client) paymentRedirect(ctx context.Context, path string, params map[string]string) (string, error) {
	headers := map[string]string{
		common.RefererHeader: "https://lantern.io/",
	}
	resp, err := a.sendProRequest(ctx, "GET", path, params, headers, nil)
	if err != nil {
		slog.Error("payment redirect", "error", err)
		return "", traces.RecordError(ctx, fmt.Errorf("payment redirect: %w", err))
	}
	var r redirect
	if err := json.Unmarshal(resp, &r); err != nil {
		return "", traces.RecordError(ctx, fmt.Errorf("unmarshaling payment redirect response: %w", err))
	}
	redirectURL := strings.TrimSpace(r.Redirect)
	if redirectURL == "" {
		return "", traces.RecordError(ctx, fmt.Errorf("payment redirect response missing redirect URL"))
	}
	return redirectURL, nil
}

// SubscriptionPaymentRedirectURL generates a redirect URL for subscription payment.
func (a *Client) SubscriptionPaymentRedirectURL(ctx context.Context, data PaymentRedirectData) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "subscription_payment_redirect_url")
	defer span.End()
	params := map[string]string{
		"provider":    data.Provider,
		"plan":        data.Plan,
		"deviceName":  data.DeviceName,
		"email":       data.Email,
		"billingType": string(data.BillingType),
	}
	if data.IdempotencyKey != "" {
		params["idempotencyKey"] = data.IdempotencyKey
	}
	return a.paymentRedirect(ctx, "/subscription-payment-redirect", params)
}

// PaymentRedirect is used to get the payment redirect URL with PaymentRedirectData.
// This is used in the desktop and android apps.
func (a *Client) PaymentRedirect(ctx context.Context, data PaymentRedirectData) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "payment_redirect")
	defer span.End()
	params := map[string]string{
		"provider":   data.Provider,
		"plan":       data.Plan,
		"deviceName": data.DeviceName,
		"email":      data.Email,
	}
	if data.IdempotencyKey != "" {
		params["idempotencyKey"] = data.IdempotencyKey
	}
	return a.paymentRedirect(ctx, "/payment-redirect", params)
}

type PurchaseResponse struct {
	*protos.BaseResponse `json:",inline"`
	PaymentStatus        string      `json:"paymentStatus"`
	Plan                 protos.Plan `json:"plan"`
	Status               string      `json:"status"`
}

// ActivationCode is used to purchase a subscription using a reseller code.
func (a *Client) ActivationCode(ctx context.Context, email, resellerCode string) (*PurchaseResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "activation_code")
	defer span.End()

	data := map[string]any{
		"idempotencyKey": strconv.FormatInt(time.Now().UnixNano(), 10),
		"provider":       "reseller-code",
		"email":          email,
		"deviceName":     settings.GetString(settings.DeviceIDKey),
		"resellerCode":   resellerCode,
	}
	resp, err := a.sendProRequest(ctx, "POST", "/purchase", nil, nil, data)
	if err != nil {
		slog.Error("retrieving subscription status", "error", err)
		return nil, traces.RecordError(ctx, fmt.Errorf("retrieving subscription status: %w", err))
	}
	var purchase PurchaseResponse
	if err := json.Unmarshal(resp, &purchase); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("unmarshaling purchase response: %w", err))
	}
	if purchase.BaseResponse != nil && purchase.Error != "" {
		slog.Error("retrieving subscription status", "error", purchase.Error)
		return nil, traces.RecordError(ctx, fmt.Errorf("received bad response: %s", purchase.Error))
	}
	return &purchase, nil
}
