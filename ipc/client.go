package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"

	box "github.com/getlantern/lantern-box"

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/issue"
	rlog "github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"

	sjson "github.com/sagernet/sing/common/json"
)

func newClient() *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:       dialContext,
				ForceAttemptHTTP2: true,
				Protocols:         &protocols,
			},
		},
	}
}

// marshalBody encodes body as a JSON reader suitable for an HTTP request body.
// Returns nil if body is nil.
func marshalBody(body any) (io.Reader, error) {
	if body == nil {
		return nil, nil
	}
	switch body := body.(type) {
	case []byte:
		return bytes.NewReader(body), nil
	default:
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		return bytes.NewReader(data), nil
	}
}

// doJSON executes an HTTP request and decodes the JSON response into dst.
func (c *Client) doJSON(ctx context.Context, method, endpoint string, body, dst any) error {
	data, err := c.do(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if dst == nil {
		return nil
	}
	return json.Unmarshal(data, dst)
}

// Error is returned by Client methods when the server responds with an error status.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("ipc: status %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether the error is a 404 response.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

/////////////
//   VPN   //
/////////////

// VPNStatus returns the current VPN connection status.
func (c *Client) VPNStatus(ctx context.Context) (vpn.VPNStatus, error) {
	var status vpn.VPNStatus
	err := c.doJSON(ctx, http.MethodGet, vpnStatusEndpoint, nil, &status)
	return status, err
}

// ConnectVPN connects the VPN using the given server tag.
func (c *Client) ConnectVPN(ctx context.Context, tag string) error {
	_, err := c.do(ctx, http.MethodPost, vpnConnectEndpoint, TagRequest{Tag: tag})
	return err
}

// DisconnectVPN disconnects the VPN.
func (c *Client) DisconnectVPN(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, vpnDisconnectEndpoint, nil)
	return err
}

// RestartVPN restarts the VPN connection.
func (c *Client) RestartVPN(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, vpnRestartEndpoint, nil)
	return err
}

// VPNConnections returns all VPN connections (active and recently closed).
func (c *Client) VPNConnections(ctx context.Context) ([]vpn.Connection, error) {
	var conns []vpn.Connection
	err := c.doJSON(ctx, http.MethodGet, vpnConnectionsEndpoint, nil, &conns)
	return conns, err
}

// ActiveVPNConnections returns currently active VPN connections.
func (c *Client) ActiveVPNConnections(ctx context.Context) ([]vpn.Connection, error) {
	var conns []vpn.Connection
	err := c.doJSON(ctx, http.MethodGet, vpnConnectionsEndpoint+"?active=true", nil, &conns)
	return conns, err
}

// RunOfflineURLTests runs URL performance tests when offline (VPN disconnected) and caches the
// results. This enables autoconnect to select the best server for the initial connection.
func (c *Client) RunOfflineURLTests(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, vpnOfflineTestsEndpoint, nil)
	return err
}

// VPNStatusEvents connects to the VPN status event stream. It calls handler for each event
// received until ctx is cancelled or the connection is closed.
func (c *Client) VPNStatusEvents(ctx context.Context, handler func(vpn.StatusUpdateEvent)) error {
	return c.sseStream(ctx, vpnStatusEventsEndpoint, func(data []byte) {
		var evt vpn.StatusUpdateEvent
		if err := json.Unmarshal(data, &evt); err != nil {
			return
		}
		handler(evt)
	})
}

///////////////////////
// Server selection  //
///////////////////////

var boxCtx = box.BaseContext()

// SelectServer selects the server with the given tag.
func (c *Client) SelectServer(ctx context.Context, tag string) error {
	_, err := c.do(ctx, http.MethodPost, serverSelectedEndpoint, TagRequest{Tag: tag})
	return err
}

// SelectedServer returns the currently selected server and whether it still exists.
func (c *Client) SelectedServer(ctx context.Context) (*servers.Server, bool, error) {
	data, err := c.do(ctx, http.MethodGet, serverSelectedEndpoint, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := sjson.UnmarshalExtendedContext[SelectedServerResponse](boxCtx, data)
	return resp.Server, resp.Exists, err
}

// SelectedServerJSON returns the currently selected server as raw JSON bytes.
func (c *Client) SelectedServerJSON(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, serverSelectedEndpoint, nil)
}

// AutoSelected returns the server that's currently auto-selected.
func (c *Client) AutoSelected(ctx context.Context) (*servers.Server, error) {
	data, err := c.do(ctx, http.MethodGet, serverAutoSelectedEndpoint, nil)
	if err != nil {
		return nil, err
	}
	return sjson.UnmarshalExtendedContext[*servers.Server](boxCtx, data)
}

// AutoSelectedEvents connects to the auto-selected event stream. It calls handler for each
// event received until ctx is cancelled or the connection is closed.
func (c *Client) AutoSelectedEvents(ctx context.Context, handler func(vpn.AutoSelectedEvent)) error {
	return c.sseStream(ctx, serverAutoSelectedEventsEndpoint, func(data []byte) {
		var evt vpn.AutoSelectedEvent
		if err := json.Unmarshal(data, &evt); err != nil {
			return
		}
		handler(evt)
	})
}

///////////////////////
// Config events     //
///////////////////////

// ConfigEvents connects to the config event stream. The server emits a frame
// on every config.NewConfigEvent; the payload is intentionally empty — callers
// should treat each frame as a "refresh" signal and fetch any state they need
// via the other GET endpoints. The handler is called once per frame received
// until ctx is cancelled or the connection is closed.
func (c *Client) ConfigEvents(ctx context.Context, handler func()) error {
	return c.sseStream(ctx, configEventsEndpoint, func(data []byte) {
		handler()
	})
}

// UpdateConfig forces an immediate config fetch on the daemon. Returns an error
// if config fetching is disabled.
func (c *Client) UpdateConfig(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, configUpdateEndpoint, nil)
	return err
}

///////////////////////
// Server management //
///////////////////////

// Servers returns all servers.
func (c *Client) Servers(ctx context.Context) ([]*servers.Server, error) {
	data, err := c.do(ctx, http.MethodGet, serversEndpoint, nil)
	if err != nil {
		return nil, err
	}
	return sjson.UnmarshalExtendedContext[[]*servers.Server](boxCtx, data)
}

// ServersJSON returns all servers as raw JSON bytes.
// This is useful when the caller needs to forward the JSON without re-marshaling,
// since the server options require sing-box's context-aware JSON encoder.
func (c *Client) ServersJSON(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, serversEndpoint, nil)
}

// GetServerByTag returns the server with the given tag.
func (c *Client) GetServerByTag(ctx context.Context, tag string) (*servers.Server, bool, error) {
	q := url.Values{"tag": {tag}}
	data, err := c.do(ctx, http.MethodGet, serversEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		if IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	server, err := sjson.UnmarshalExtendedContext[*servers.Server](boxCtx, data)
	if err != nil {
		return nil, false, err
	}
	return server, true, nil
}

// GetServerByTagJSON returns the server with the given tag as raw JSON bytes.
func (c *Client) GetServerByTagJSON(ctx context.Context, tag string) ([]byte, bool, error) {
	q := url.Values{"tag": {tag}}
	data, err := c.do(ctx, http.MethodGet, serversEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		if IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

// AddServers adds servers.
func (c *Client) AddServers(ctx context.Context, list servers.ServerList) error {
	req := AddServersRequest{Servers: list}
	body, err := sjson.MarshalContext(boxCtx, req)
	if err != nil {
		return fmt.Errorf("marshal add servers request: %w", err)
	}
	_, err = c.do(ctx, http.MethodPost, serversAddEndpoint, body)
	return err
}

// RemoveServers removes servers by tag from the given group.
func (c *Client) RemoveServers(ctx context.Context, tags []string) error {
	_, err := c.do(ctx, http.MethodPost, serversRemoveEndpoint, RemoveServersRequest{Tags: tags})
	return err
}

// AddServersByJSON adds servers from a JSON configuration string and returns the tags of the added servers.
func (c *Client) AddServersByJSON(ctx context.Context, config string) ([]string, error) {
	data, err := c.do(ctx, http.MethodPost, serversFromJSONEndpoint, JSONConfigRequest{Config: config})
	if err != nil {
		return nil, err
	}
	var tags []string
	if err := json.Unmarshal(data, &tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// AddServersByURL adds servers from the given URLs and returns the tags of the added servers.
func (c *Client) AddServersByURL(ctx context.Context, urls []string, skipCertVerification bool) ([]string, error) {
	data, err := c.do(ctx, http.MethodPost, serversFromURLsEndpoint, URLsRequest{URLs: urls, SkipCertVerification: skipCertVerification})
	if err != nil {
		return nil, err
	}
	var tags []string
	if err := json.Unmarshal(data, &tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// AddPrivateServer adds a private server.
func (c *Client) AddPrivateServer(ctx context.Context, tag, ip string, port int, accessToken string) error {
	_, err := c.do(ctx, http.MethodPost, serversPrivateEndpoint, PrivateServerRequest{Tag: tag, IP: ip, Port: port, AccessToken: accessToken})
	return err
}

// InviteToPrivateServer creates an invite for a private server and returns the invite code.
func (c *Client) InviteToPrivateServer(ctx context.Context, ip string, port int, accessToken, inviteName string) (string, error) {
	var resp CodeResponse
	err := c.doJSON(ctx, http.MethodPost, serversPrivateInviteEndpoint,
		PrivateServerInviteRequest{IP: ip, Port: port, AccessToken: accessToken, InviteName: inviteName}, &resp)
	return resp.Code, err
}

// RevokePrivateServerInvite revokes an invite for a private server.
func (c *Client) RevokePrivateServerInvite(ctx context.Context, ip string, port int, accessToken, inviteName string) error {
	_, err := c.do(ctx, http.MethodDelete, serversPrivateInviteEndpoint,
		PrivateServerInviteRequest{IP: ip, Port: port, AccessToken: accessToken, InviteName: inviteName})
	return err
}

//////////////
// Settings //
//////////////

// Features returns the feature flags from the current configuration.
func (c *Client) Features(ctx context.Context) (map[string]bool, error) {
	var features map[string]bool
	err := c.doJSON(ctx, http.MethodGet, featuresEndpoint, nil, &features)
	return features, err
}

// Settings returns the current settings as a map of key-value pairs.
func (c *Client) Settings(ctx context.Context) (settings.Settings, error) {
	var s settings.Settings
	err := c.doJSON(ctx, http.MethodGet, settingsEndpoint, nil, &s)
	return s, err
}

// PatchSettings updates settings with the given key-value pairs and returns the full updates settings.
func (c *Client) PatchSettings(ctx context.Context, updates settings.Settings) (settings.Settings, error) {
	var s settings.Settings
	err := c.doJSON(ctx, http.MethodPatch, settingsEndpoint, updates, &s)
	return s, err
}

func (c *Client) EnableTelemetry(ctx context.Context, enable bool) error {
	_, err := c.PatchSettings(ctx, settings.Settings{settings.TelemetryKey: enable})
	return err
}

func (c *Client) EnableSplitTunneling(ctx context.Context, enable bool) error {
	_, err := c.PatchSettings(ctx, settings.Settings{settings.SplitTunnelKey: enable})
	return err
}

func (c *Client) EnableSmartRouting(ctx context.Context, enable bool) error {
	_, err := c.PatchSettings(ctx, settings.Settings{settings.SmartRoutingKey: enable})
	return err
}

func (c *Client) EnableAdBlocking(ctx context.Context, enable bool) error {
	_, err := c.PatchSettings(ctx, settings.Settings{settings.AdBlockKey: enable})
	return err
}

// EnableConfigFetch toggles periodic config fetching. Passing false sets
// settings.ConfigFetchDisabledKey to true on the daemon.
func (c *Client) EnableConfigFetch(ctx context.Context, enable bool) error {
	_, err := c.PatchSettings(ctx, settings.Settings{settings.ConfigFetchDisabledKey: !enable})
	return err
}

// SetLogLevel sets the daemon's log level. Valid values: trace, debug, info,
// warn, error, fatal, panic, disable.
func (c *Client) SetLogLevel(ctx context.Context, level string) error {
	if _, err := rlog.ParseLogLevel(level); err != nil {
		return err
	}
	_, err := c.PatchSettings(ctx, settings.Settings{settings.LogLevelKey: level})
	return err
}

/////////
// Env //
/////////

// PatchEnvVars updates the daemon's in-memory environment variables.
// This is intended for dev/testing use only.
func (c *Client) PatchEnvVars(ctx context.Context, updates map[string]string) (map[string]string, error) {
	var result map[string]string
	err := c.doJSON(ctx, http.MethodPatch, envEndpoint, updates, &result)
	return result, err
}

//////////////////
// Split Tunnel //
/////////////////

// SplitTunnelFilters returns the current split tunnel configuration.
func (c *Client) SplitTunnelFilters(ctx context.Context) (vpn.SplitTunnelFilter, error) {
	var filter vpn.SplitTunnelFilter
	err := c.doJSON(ctx, http.MethodGet, splitTunnelEndpoint, nil, &filter)
	return filter, err
}

// AddSplitTunnelItems adds items to the split tunnel filter.
func (c *Client) AddSplitTunnelItems(ctx context.Context, items vpn.SplitTunnelFilter) error {
	_, err := c.do(ctx, http.MethodPost, splitTunnelEndpoint, items)
	return err
}

// RemoveSplitTunnelItems removes items from the split tunnel filter.
func (c *Client) RemoveSplitTunnelItems(ctx context.Context, items vpn.SplitTunnelFilter) error {
	_, err := c.do(ctx, http.MethodDelete, splitTunnelEndpoint, items)
	return err
}

/////////////
// Account //
/////////////

// NewUser creates a new anonymous user.
func (c *Client) NewUser(ctx context.Context) (*account.UserData, error) {
	var userData account.UserData
	if err := c.doJSON(ctx, http.MethodPost, accountNewUserEndpoint, nil, &userData); err != nil {
		return nil, err
	}
	return &userData, nil
}

// Login authenticates the user with email and password.
func (c *Client) Login(ctx context.Context, email, password string) (*account.UserData, error) {
	var userData account.UserData
	err := c.doJSON(ctx, http.MethodPost, accountLoginEndpoint,
		EmailPasswordRequest{Email: email, Password: password}, &userData)
	if err != nil {
		return nil, err
	}
	return &userData, nil
}

// Logout logs the user out.
func (c *Client) Logout(ctx context.Context, email string) (*account.UserData, error) {
	var userData account.UserData
	if err := c.doJSON(ctx, http.MethodPost, accountLogoutEndpoint, EmailRequest{Email: email}, &userData); err != nil {
		return nil, err
	}
	return &userData, nil
}

// FetchUserData fetches fresh user data from the remote server.
func (c *Client) FetchUserData(ctx context.Context) (*account.UserData, error) {
	return c.userData(ctx, true)
}

// UserData returns locally cached user data.
func (c *Client) UserData(ctx context.Context) (*account.UserData, error) {
	return c.userData(ctx, false)
}

func (c *Client) userData(ctx context.Context, fetch bool) (*account.UserData, error) {
	var userData account.UserData
	url := fmt.Sprintf("%s?fetch=%v", accountUserDataEndpoint, fetch)
	if err := c.doJSON(ctx, http.MethodGet, url, nil, &userData); err != nil {
		return nil, err
	}
	return &userData, nil
}

// UserDevices returns the list of devices linked to the user's account.
func (c *Client) UserDevices(ctx context.Context) ([]settings.Device, error) {
	var devices []settings.Device
	err := c.doJSON(ctx, http.MethodGet, accountDevicesEndpoint, nil, &devices)
	return devices, err
}

// RemoveDevice removes a device from the user's account.
func (c *Client) RemoveDevice(ctx context.Context, deviceID string) (*account.LinkResponse, error) {
	var resp account.LinkResponse
	if err := c.doJSON(ctx, http.MethodDelete, accountDevicesEndpoint+url.PathEscape(deviceID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SignUp creates a new account with the given email and password.
func (c *Client) SignUp(ctx context.Context, email, password string) ([]byte, *account.SignupResponse, error) {
	var resp SignupResponse
	err := c.doJSON(
		ctx, http.MethodPost, accountSignupEndpoint,
		EmailPasswordRequest{Email: email, Password: password}, &resp,
	)
	if err != nil {
		return nil, nil, err
	}
	return resp.Salt, resp.Response, nil
}

// SignupEmailConfirmation confirms the signup email with the given code.
func (c *Client) SignupEmailConfirmation(ctx context.Context, email, code string) error {
	_, err := c.do(ctx, http.MethodPost, accountSignupEndpoint+"confirm", EmailCodeRequest{Email: email, Code: code})
	return err
}

// SignupEmailResendCode requests a resend of the signup confirmation email.
func (c *Client) SignupEmailResendCode(ctx context.Context, email string) error {
	_, err := c.do(ctx, http.MethodPost, accountSignupEndpoint+"resend", EmailRequest{Email: email})
	return err
}

// StartChangeEmail initiates an email address change.
func (c *Client) StartChangeEmail(ctx context.Context, newEmail, password string) error {
	_, err := c.do(ctx, http.MethodPost, accountEmailEndpoint+"/start", ChangeEmailStartRequest{NewEmail: newEmail, Password: password})
	return err
}

// CompleteChangeEmail completes an email address change.
func (c *Client) CompleteChangeEmail(ctx context.Context, newEmail, password, code string) error {
	_, err := c.do(ctx, http.MethodPost, accountEmailEndpoint+"/complete",
		ChangeEmailCompleteRequest{NewEmail: newEmail, Password: password, Code: code})
	return err
}

// StartRecoveryByEmail initiates account recovery by email.
func (c *Client) StartRecoveryByEmail(ctx context.Context, email string) error {
	_, err := c.do(ctx, http.MethodPost, accountRecoveryEndpoint+"/start", EmailRequest{Email: email})
	return err
}

// CompleteRecoveryByEmail completes account recovery with a new password and code.
func (c *Client) CompleteRecoveryByEmail(ctx context.Context, email, newPassword, code string) error {
	_, err := c.do(ctx, http.MethodPost, accountRecoveryEndpoint+"/complete",
		RecoveryCompleteRequest{Email: email, NewPassword: newPassword, Code: code})
	return err
}

// ValidateEmailRecoveryCode validates the recovery code without completing the recovery.
func (c *Client) ValidateEmailRecoveryCode(ctx context.Context, email, code string) error {
	_, err := c.do(ctx, http.MethodPost, accountRecoveryEndpoint+"/validate", EmailCodeRequest{Email: email, Code: code})
	return err
}

// DeleteAccount deletes the user's account.
func (c *Client) DeleteAccount(ctx context.Context, email, password string) (*account.UserData, error) {
	var userData account.UserData
	err := c.doJSON(ctx, http.MethodDelete, accountDeleteEndpoint,
		EmailPasswordRequest{Email: email, Password: password}, &userData)
	if err != nil {
		return nil, err
	}
	return &userData, nil
}

// OAuthLoginURL returns the OAuth login URL for the given provider.
func (c *Client) OAuthLoginURL(ctx context.Context, provider string) (string, error) {
	var resp URLResponse
	q := url.Values{"provider": {provider}}
	err := c.doJSON(ctx, http.MethodGet, accountOAuthEndpoint+"?"+q.Encode(), nil, &resp)
	return resp.URL, err
}

// OAuthLoginCallback exchanges an OAuth token for user data.
func (c *Client) OAuthLoginCallback(ctx context.Context, oAuthToken string) (*account.UserData, error) {
	var userData account.UserData
	err := c.doJSON(ctx, http.MethodPost, accountOAuthEndpoint,
		OAuthTokenRequest{OAuthToken: oAuthToken}, &userData)
	if err != nil {
		return nil, err
	}
	return &userData, nil
}

// DataCapInfo returns the current data cap information as a JSON string.
func (c *Client) DataCapInfo(ctx context.Context) (*account.DataCapInfo, error) {
	var resp account.DataCapInfo
	err := c.doJSON(ctx, http.MethodGet, accountDataCapEndpoint, nil, &resp)
	return &resp, err
}

// DataCapStream connects to the data cap event stream. It calls handler for each event
// received until ctx is cancelled or the connection is closed.
func (c *Client) DataCapStream(ctx context.Context, handler func(account.DataCapInfo)) error {
	return c.sseStream(ctx, accountDataCapStreamEndpoint, func(data []byte) {
		var info account.DataCapInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return
		}
		handler(info)
	})
}

///////////////////
// Subscriptions //
///////////////////

// ActivationCode purchases a subscription using a reseller code.
func (c *Client) ActivationCode(ctx context.Context, email, resellerCode string) (*account.PurchaseResponse, error) {
	var resp account.PurchaseResponse
	err := c.doJSON(ctx, http.MethodPost, subscriptionActivationEndpoint,
		ActivationRequest{Email: email, ResellerCode: resellerCode}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// NewStripeSubscription creates a new Stripe subscription and returns the client secret.
func (c *Client) NewStripeSubscription(ctx context.Context, email, planID string) (string, error) {
	var resp ClientSecretResponse
	err := c.doJSON(ctx, http.MethodPost, subscriptionStripeEndpoint,
		StripeSubscriptionRequest{Email: email, PlanID: planID}, &resp)
	return resp.ClientSecret, err
}

// ReferralAttach attaches a referral code to the current user.
func (c *Client) ReferralAttach(ctx context.Context, code string) (bool, error) {
	var resp SuccessResponse
	err := c.doJSON(ctx, http.MethodPost, subscriptionReferralEndpoint, CodeRequest{Code: code}, &resp)
	return resp.Success, err
}

// StripeBillingPortalURL returns the Stripe billing portal URL.
func (c *Client) StripeBillingPortalURL(ctx context.Context) (string, error) {
	var resp URLResponse
	err := c.doJSON(ctx, http.MethodGet, subscriptionBillingPortalEndpoint, nil, &resp)
	return resp.URL, err
}

// PaymentRedirect returns a payment redirect URL.
func (c *Client) PaymentRedirect(ctx context.Context, data account.PaymentRedirectData) (string, error) {
	var resp URLResponse
	err := c.doJSON(ctx, http.MethodPost, subscriptionPaymentRedirectEndpoint, data, &resp)
	return resp.URL, err
}

// SubscriptionPaymentRedirectURL returns a subscription payment redirect URL.
func (c *Client) SubscriptionPaymentRedirectURL(ctx context.Context, data account.PaymentRedirectData) (string, error) {
	var resp URLResponse
	err := c.doJSON(ctx, http.MethodPost, subscriptionPaymentRedirectURLEndpoint, data, &resp)
	return resp.URL, err
}

// SubscriptionPlans returns available subscription plans for the given channel.
func (c *Client) SubscriptionPlans(ctx context.Context, channel string) (string, error) {
	var resp PlansResponse
	q := url.Values{"channel": {channel}}
	err := c.doJSON(ctx, http.MethodGet, subscriptionPlansEndpoint+"?"+q.Encode(), nil, &resp)
	return resp.Plans, err
}

// VerifySubscription verifies a subscription purchase.
func (c *Client) VerifySubscription(ctx context.Context, service account.SubscriptionService, data map[string]string) (string, error) {
	var resp ResultResponse
	err := c.doJSON(ctx, http.MethodPost, subscriptionVerifyEndpoint,
		VerifySubscriptionRequest{Service: service, Data: data}, &resp)
	return resp.Result, err
}

///////////
// Issue //
///////////

// ReportIssue submits an issue report. additionalAttachments is a list of file paths for additional
// files to include. Logs, diagnostics, and the config response are included automatically and do
// not need to be specified.
func (c *Client) ReportIssue(ctx context.Context, issueType issue.IssueType, description, email string, additionalAttachments []string) error {
	_, err := c.do(ctx, http.MethodPost, issueEndpoint,
		IssueReportRequest{IssueType: issueType, Description: description, Email: email, AdditionalAttachments: additionalAttachments})
	return err
}

/////////////
// helpers //
/////////////

// isConnectionError reports whether err indicates that the IPC socket is unreachable
// (e.g. connection refused or socket file not found).
func isConnectionError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// connection refused (server not listening)
		if errors.Is(opErr.Err, syscall.ECONNREFUSED) {
			return true
		}
		// socket file does not exist (server never started / was cleaned up)
		if errors.Is(opErr.Err, syscall.ENOENT) {
			return true
		}
		// check wrapped syscall errors
		var sysErr *os.SyscallError
		if errors.As(opErr.Err, &sysErr) {
			return errors.Is(sysErr.Err, syscall.ECONNREFUSED) || errors.Is(sysErr.Err, syscall.ENOENT)
		}
	}
	// Also check the unwrapped error directly for cases where the wrapping differs by platform
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}
