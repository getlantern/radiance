package pro

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/user/protos"
	"github.com/go-resty/resty/v2"
)

// Pro represents pro server apis
type Pro struct {
	proClient ProClient
}

// New returns the object handling anything user-account related
func New(httpClient *http.Client, userConfig common.UserConfig) *Pro {
	opts := common.Opts{
		HttpClient: httpClient,
		BaseURL:    common.ProServerUrl,
		OnBeforeRequest: func(client *resty.Client, req *http.Request) error {
			// Add any headers or modifications to the request here
			req.Header.Set(backend.AppNameHeader, app.Name)
			req.Header.Set(backend.VersionHeader, app.Version)
			req.Header.Set(backend.PlatformHeader, app.Platform)
			req.Header.Set(backend.DeviceIDHeader, userConfig.DeviceID())
			if userConfig.LegacyToken() != "" {
				req.Header.Set(backend.ProTokenHeader, userConfig.LegacyToken())
			}
			if userConfig.LegacyID() != 0 {
				req.Header.Set(backend.UserIDHeader, strconv.FormatInt(userConfig.LegacyID(), 10))
			}

			return nil
		},
	}
	return &Pro{
		proClient: &proClient{
			WebClient:  common.NewWebClient(&opts),
			UserConfig: userConfig,
		},
	}
}

// Create a new user account
func (u *Pro) UserCreate(ctx context.Context) (*protos.UserDataResponse, error) {
	return u.proClient.UserCreate(ctx)

}

// SubscriptionPaymentRedirect creates a new subscription with url
func (u *Pro) SubscriptionPaymentRedirect(ctx context.Context, data *protos.SubscriptionPaymentRedirectRequest) (*protos.SubscriptionPaymentRedirectResponse, error) {
	return u.proClient.SubscriptionPaymentRedirect(ctx, data)
}

// StripeSubscription creates a new subscription using Stripe SDK
// For Android only
func (u *Pro) StripeSubscription(ctx context.Context, data *protos.SubscriptionRequest) (*protos.SubscriptionResponse, error) {
	slog.Debug("StripeSubscription", "data", data)
	resp, err := u.proClient.StripeSubscription(ctx, data)
	if err != nil {
		slog.Error("Error in StripeSubscription", "error", err)
		return nil, err
	}
	return resp, nil
}
