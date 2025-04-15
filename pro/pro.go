package pro

import (
	"context"
	"net/http"

	"github.com/getlantern/radiance/common"
)

// Pro represents pro server apis
type Pro struct {
	proClient ProClient
}

// New returns the object handling anything user-account related
func New(httpClient *http.Client) *Pro {
	opts := common.Opts{
		HttpClient: httpClient,
		BaseURL:    common.ProServerUrl,
	}
	return &Pro{
		proClient: &proClient{
			WebClient: common.NewWebClient(&opts),
		},
	}
}

// SignUpEmailResendCode requests that the sign-up code be resent via email.
func (u *Pro) SubscriptionPaymentRedirect(ctx context.Context, data *SubscriptionPaymentRedirectRequest) (any, error) {
	return u.proClient.SubscriptionPaymentRedirect(ctx, data)
}
