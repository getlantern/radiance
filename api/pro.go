package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
	"github.com/go-resty/resty/v2"
)

// Pro represents pro server apis
type Pro struct {
	proClient ProClient
	userInfo  common.UserInfo
}

// New returns the object handling anything pro-server related
func NewPro(httpClient *http.Client, userInfo common.UserInfo) *Pro {
	opts := common.WebClientOptions{
		HttpClient: httpClient,
		BaseURL:    common.ProServerUrl,
		OnBeforeRequest: func(client *resty.Client, req *http.Request) error {
			// Add any headers or modifications to the request here
			req.Header.Set(backend.AppNameHeader, app.Name)
			req.Header.Set(backend.VersionHeader, app.Version)
			req.Header.Set(backend.PlatformHeader, app.Platform)
			req.Header.Set(backend.DeviceIDHeader, userInfo.DeviceID())
			if userInfo.LegacyToken() != "" {
				req.Header.Set(backend.ProTokenHeader, userInfo.LegacyToken())
			}
			if userInfo.LegacyID() != 0 {
				req.Header.Set(backend.UserIDHeader, strconv.FormatInt(userInfo.LegacyID(), 10))
			}
			if req.URL != nil && strings.HasSuffix(req.URL.Path, "/subscription-payment-redirect") {
				req.Header.Set(backend.RefererHeader, "https://lantern.io/")
			}
			return nil
		},
	}
	return &Pro{
		userInfo: userInfo,
		proClient: &proClient{
			WebClient: common.NewWebClient(&opts),
			UserInfo:  userInfo,
		},
	}
}

// Create a new user account
func (u *Pro) UserCreate(ctx context.Context) (*protos.UserDataResponse, error) {
	return u.proClient.CreateUser(ctx)
}

// UserData returns the user data
func (u *Pro) UserData(ctx context.Context) (*protos.UserDataResponse, error) {
	resp, err := u.proClient.UserData(ctx)
	if err != nil {
		slog.Error("Error in UserData", "error", err)
		return nil, err
	}
	return resp, nil
}

// SubscriptionPaymentRedirect creates a new subscription with url
func (u *Pro) SubscriptionPaymentRedirect(ctx context.Context, data *protos.SubscriptionPaymentRedirectRequest) (*protos.SubscriptionPaymentRedirectResponse, error) {
	return u.proClient.SubscriptionPaymentRedirect(ctx, data)
}

// StripeSubscription creates a new subscription using Stripe SDK
// For Android only
func (u *Pro) StripeSubscription(ctx context.Context, data *protos.SubscriptionRequest) (*protos.SubscriptionResponse, error) {
	resp, err := u.proClient.StripeSubscription(ctx, data)
	if err != nil {
		slog.Error("Error in StripeSubscription", "error", err)
		return nil, err
	}
	return resp, nil
}

// Plans returns the list of plans
func (u *Pro) Plans(ctx context.Context, channel string) (*protos.PlansResponse, error) {
	resp, err := u.proClient.Plans(ctx, channel)
	if err != nil {
		slog.Error("Error in Plans", "error", err)
		return nil, err
	}
	if resp.BaseResponse != nil && resp.BaseResponse.Error != "" {
		slog.Error("Error in Plans", "error", resp.BaseResponse.Error)
		return nil, fmt.Errorf("error in Plans: %s", resp.BaseResponse.Error)
	}
	return resp, nil
}

func (u *Pro) StripeBillingPortalUrl() (*protos.SubscriptionPaymentRedirectResponse, error) {
	portalUrl, err := url.Parse(fmt.Sprintf("%s/%s", common.ProServerUrl, "stripe-billing-portal"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}
	query := portalUrl.Query()
	query.Set("referer", "https://lantern.io/")
	query.Set("userId", strconv.FormatInt(u.userInfo.LegacyID(), 10))
	portalUrl.RawQuery = query.Encode()

	return &protos.SubscriptionPaymentRedirectResponse{
		Redirect: portalUrl.String(),
	}, nil
}

// GoogleSubscription creates a new subscription using Google Play SDK
// For Android only
func (u *Pro) GoogleSubscription(ctx context.Context, purchaseToken, planId string) (*protos.AcknowledgmentResponse, error) {
	resp, err := u.proClient.GoogleSubscription(ctx, purchaseToken, planId)
	if err != nil {
		slog.Error("Error in GoogleSubscription", "error", err)
		return nil, err
	}
	return resp, nil
}

func (u *Pro) AppleSubscription(ctx context.Context, purchaseToken, planId string) (*protos.AcknowledgmentResponse, error) {
	resp, err := u.proClient.AppleSubscription(ctx, purchaseToken, planId)
	if err != nil {
		slog.Error("error in apple subscription", "error", err)
		return nil, err
	}
	return resp, nil
}

func (u *Pro) PaymentRedirect(ctx context.Context, data *protos.PaymentRedirectRequest) (*protos.SubscriptionPaymentRedirectResponse, error) {
	resp, err := u.proClient.PaymentRedirect(ctx, data)
	if err != nil {
		slog.Error("Error in PaymentRedirect", "error", err)
		return nil, err
	}
	return resp, nil
}
