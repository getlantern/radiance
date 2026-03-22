package ipc

import (
	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/issue"
	"github.com/getlantern/radiance/servers"
)

// Shared request types used by both client and server.

type TagRequest struct {
	Tag string `json:"tag"`
}

type EmailRequest struct {
	Email string `json:"email"`
}

type EmailPasswordRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type EmailCodeRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type OAuthTokenRequest struct {
	OAuthToken string `json:"oAuthToken"`
}

type CodeRequest struct {
	Code string `json:"code"`
}

type JSONConfigRequest struct {
	Config string `json:"config"`
}

type AddServersRequest struct {
	Group   servers.ServerGroup `json:"group"`
	Options servers.Options     `json:"options"`
}

type RemoveServersRequest struct {
	Tags []string `json:"tags"`
}

type URLsRequest struct {
	URLs                 []string `json:"urls"`
	SkipCertVerification bool     `json:"skipCertVerification"`
}

type PrivateServerRequest struct {
	Tag         string `json:"tag"`
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	AccessToken string `json:"accessToken"`
}

type PrivateServerInviteRequest struct {
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	AccessToken string `json:"accessToken"`
	InviteName  string `json:"inviteName"`
}

type ChangeEmailStartRequest struct {
	NewEmail string `json:"newEmail"`
	Password string `json:"password"`
}

type ChangeEmailCompleteRequest struct {
	NewEmail string `json:"newEmail"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

type RecoveryCompleteRequest struct {
	Email       string `json:"email"`
	NewPassword string `json:"newPassword"`
	Code        string `json:"code"`
}

type ActivationRequest struct {
	Email        string `json:"email"`
	ResellerCode string `json:"resellerCode"`
}

type StripeSubscriptionRequest struct {
	Email  string `json:"email"`
	PlanID string `json:"planID"`
}

type VerifySubscriptionRequest struct {
	Service account.SubscriptionService `json:"service"`
	Data    map[string]string           `json:"data"`
}

type IssueReportRequest struct {
	IssueType             issue.IssueType `json:"issueType"`
	Description           string          `json:"description"`
	Email                 string          `json:"email"`
	AdditionalAttachments []string        `json:"additionalAttachments"`
}

// Shared response types used by both client and server.

type SelectedServerResponse struct {
	Server servers.Server `json:"server"`
	Exists bool           `json:"exists"`
}

type SignupResponse struct {
	Salt     []byte                  `json:"salt"`
	Response *account.SignupResponse `json:"response"`
}

type URLResponse struct {
	URL string `json:"url"`
}

type CodeResponse struct {
	Code string `json:"code"`
}

type InfoResponse struct {
	Info string `json:"info"`
}

type ClientSecretResponse struct {
	ClientSecret string `json:"clientSecret"`
}

type SuccessResponse struct {
	Success bool `json:"success"`
}

type PlansResponse struct {
	Plans string `json:"plans"`
}

type ResultResponse struct {
	Result string `json:"result"`
}
