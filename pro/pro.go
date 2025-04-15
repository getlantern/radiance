package pro

import (
	"context"
	"net/http"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/user"
	"github.com/go-resty/resty/v2"
)

// Pro represents pro server apis
type Pro struct {
	proClient ProClient
	headers   *HeaderStore
}
type HeaderStore struct {
	Token    string
	UserID   string
	DeviceID string
}

// New returns the object handling anything user-account related
func New(httpClient *http.Client, deviceId string) *Pro {
	headerStore := &HeaderStore{
		DeviceID: deviceId,
	}
	opts := common.Opts{
		HttpClient: httpClient,
		BaseURL:    common.ProServerUrl,
		OnBeforeRequest: func(client *resty.Client, req *http.Request) error {
			// Add any headers or modifications to the request here
			req.Header.Set(common.AppHeader, app.Name)
			req.Header.Set(common.Version, app.Version)
			req.Header.Set(common.PlatformHeader, app.Platform)
			req.Header.Set(common.DeviceIdHeader, headerStore.DeviceID)
			if headerStore.Token != "" {
				req.Header.Set(common.TokenHeader, headerStore.Token)
			}
			if headerStore.UserID != "" {
				req.Header.Set(common.UserIdHeader, headerStore.UserID)
			}

			return nil
		},
	}
	return &Pro{
		proClient: &proClient{
			WebClient: common.NewWebClient(&opts),
		},
		headers: headerStore,
	}
}

func (p *Pro) SetHeaders(token, userID string) {
	p.headers.Token = token
	p.headers.UserID = userID
}

// Create a new user account
func (u *Pro) UserCreate(ctx context.Context) (*user.UserResponse, error) {
	return u.proClient.UserCreate(ctx)
}

// SignUpEmailResendCode requests that the sign-up code be resent via email.
func (u *Pro) SubscriptionPaymentRedirect(ctx context.Context, data *SubscriptionPaymentRedirectRequest) (*SubscriptionPaymentRedirectResponse, error) {
	return u.proClient.SubscriptionPaymentRedirect(ctx, data)
}
