package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/r3labs/sse/v2"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/traces"
)

const saltFileName = ".salt"

// UserDataResponse represents the response from pro server
type UserDataResponse struct {
	*protos.BaseResponse           `json:",inline"`
	*protos.LoginResponse_UserData `json:",inline"`
}

type SignupResponse = protos.SignupResponse
type UserData = protos.LoginResponse

// NewUser creates a new user account
func (a *Client) NewUser(ctx context.Context) (*UserData, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "new_user")
	defer span.End()

	resp, err := a.sendProRequest(ctx, "POST", "/user-create", nil, nil, nil)
	if err != nil {
		slog.Error("creating new user", "error", err)
		return nil, traces.RecordError(ctx, err)
	}
	var userResp UserDataResponse
	if err := json.Unmarshal(resp, &userResp); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("error unmarshalling new user response: %w", err))
	}
	userData, err := a.storeData(ctx, userResp)
	if err != nil {
		return nil, err
	}
	return userData, nil
}

// FetchUserData fetches user data from the server.
func (a *Client) FetchUserData(ctx context.Context) (*UserData, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "fetch_user_data")
	defer span.End()
	return a.fetchUserData(ctx)
}

// fetchUserData calls the /user-data endpoint and stores the result via storeData.
func (a *Client) fetchUserData(ctx context.Context) (*UserData, error) {
	resp, err := a.sendProRequest(ctx, "GET", "/user-data", nil, nil, nil)
	if err != nil {
		slog.Error("user data", "error", err)
		return nil, traces.RecordError(ctx, fmt.Errorf("getting user data: %w", err))
	}
	var userResp UserDataResponse
	if err := json.Unmarshal(resp, &userResp); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("error unmarshalling new user response: %w", err))
	}
	return a.storeData(ctx, userResp)
}

func (a *Client) storeData(ctx context.Context, resp UserDataResponse) (*UserData, error) {
	if resp.BaseResponse != nil && resp.Error != "" {
		err := fmt.Errorf("received bad response: %s", resp.Error)
		slog.Error("user data", "error", err)
		return nil, traces.RecordError(ctx, err)
	}
	if resp.LoginResponse_UserData == nil {
		slog.Error("user data", "error", "no user data in response")
		return nil, traces.RecordError(ctx, fmt.Errorf("no user data in response"))
	}
	resp.DeviceID = settings.GetString(settings.DeviceIDKey)
	login := &UserData{
		LegacyID:       resp.UserId,
		LegacyToken:    resp.Token,
		LegacyUserData: resp.LoginResponse_UserData,
	}
	a.setData(login)
	return login, nil
}

// DataCapUsageResponse represents the data cap usage response
type DataCapUsageResponse struct {
	// Whether data cap is enabled for this device/user
	Enabled bool `json:"enabled"`
	// Data cap usage details (only populated if enabled is true)
	Usage *DataCapUsageDetails `json:"usage,omitempty"`
}

// DataCapUsageDetails contains details of the data cap usage
type DataCapUsageDetails struct {
	BytesAllotted      string `json:"bytesAllotted"`
	BytesUsed          string `json:"bytesUsed"`
	AllotmentStartTime string `json:"allotmentStartTime"`
	AllotmentEndTime   string `json:"allotmentEndTime"`
}

// DataCapInfo returns information about this user's data cap
func (a *Client) DataCapInfo(ctx context.Context) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "data_cap_info")
	defer span.End()

	getURL := "/datacap/" + settings.GetString(settings.DeviceIDKey)
	resp, err := a.sendRequest(ctx, "GET", getURL, nil, nil, nil)
	if err != nil {
		return "", traces.RecordError(ctx, fmt.Errorf("getting datacap info: %w", err))
	}
	var usage *DataCapUsageResponse
	if err := json.Unmarshal(resp, &usage); err != nil {
		return "", traces.RecordError(ctx, fmt.Errorf("error unmarshalling datacap info response: %w", err))
	}
	return string(resp), nil
}

type DataCapChangeEvent struct {
	events.Event
	*DataCapUsageResponse
}

// DataCapStream connects to the datacap SSE endpoint and continuously reads events.
// It sends events whenever there is an update in datacap usage with DataCapChangeEvent.
// To receive those events use events.Subscribe(&DataCapChangeEvent{}, func(evt DataCapChangeEvent) { ... })
func (a *Client) DataCapStream(ctx context.Context) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "data_cap_info_stream")
	defer span.End()

	getURL := "/stream/datacap/" + settings.GetString(settings.DeviceIDKey)
	fullURL := a.baseURL() + getURL
	sseClient := sse.NewClient(fullURL)
	sseClient.Headers = map[string]string{
		common.ContentTypeHeader: "application/json",
		common.AcceptHeader:      "text/event-stream",
		common.AppNameHeader:     common.Name,
		common.VersionHeader:     common.Version,
		common.PlatformHeader:    common.Platform,
	}
	if a.httpClient != nil {
		sseClient.Connection.Transport = a.httpClient.Transport
	}
	// Connection callbacks
	sseClient.OnConnect(func(c *sse.Client) {
		slog.Debug("Connected to datacap stream")
	})

	sseClient.OnDisconnect(func(c *sse.Client) {
		slog.Debug("Disconnected from datacap stream")
	})
	// Start listening to events
	return sseClient.SubscribeRawWithContext(ctx, func(msg *sse.Event) {
		eventType := string(msg.Event)
		data := msg.Data
		switch eventType {
		case "datacap":
			var datacap DataCapUsageResponse
			err := json.Unmarshal(data, &datacap)
			if err != nil {
				slog.Error("datacap stream unmarshal error", "error", err)
				return
			}
			events.Emit(DataCapChangeEvent{DataCapUsageResponse: &datacap})
		case "cap_exhausted":
			slog.Warn("Datacap exhausted ")
			return
		default:
			// Heartbeat or unknown event - silently ignore
		}
	})
}

// SignUp signs the user up for an account.
func (a *Client) SignUp(ctx context.Context, email, password string) ([]byte, *protos.SignupResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "sign_up")
	defer span.End()

	lowerCaseEmail := strings.ToLower(email)
	salt, err := generateSalt()
	if err != nil {
		return nil, nil, traces.RecordError(ctx, err)
	}
	srpClient, err := newSRPClient(lowerCaseEmail, password, salt)
	if err != nil {
		return nil, nil, traces.RecordError(ctx, err)
	}
	verifierKey, err := srpClient.Verifier()
	if err != nil {
		return nil, nil, traces.RecordError(ctx, err)
	}
	data := &protos.SignupRequest{
		Email:                 lowerCaseEmail,
		Salt:                  salt,
		Verifier:              verifierKey.Bytes(),
		SkipEmailConfirmation: true,
		// Set temp always to true for now
		// If new user faces any issue while sign up user can sign up again
		Temp: true,
	}

	resp, err := a.sendRequest(ctx, "POST", "/users/signup", nil, nil, data)
	if err != nil {
		return nil, nil, traces.RecordError(ctx, err)
	}
	a.setSalt(salt)

	var signupData protos.SignupResponse
	if err := proto.Unmarshal(resp, &signupData); err != nil {
		return nil, nil, traces.RecordError(ctx, fmt.Errorf("error unmarshalling sign up response: %w", err))
	}
	idErr := settings.Set(settings.UserIDKey, signupData.LegacyID)
	if idErr != nil {
		return nil, nil, traces.RecordError(ctx, fmt.Errorf("could not save user id: %w", idErr))
	}
	proTokenErr := settings.Set(settings.TokenKey, signupData.ProToken)
	if proTokenErr != nil {
		return nil, nil, traces.RecordError(ctx, fmt.Errorf("could not save token: %w", proTokenErr))
	}
	jwtTokenErr := settings.Set(settings.JwtTokenKey, signupData.Token)
	if jwtTokenErr != nil {
		return nil, nil, traces.RecordError(ctx, fmt.Errorf("could not save JWT token: %w", jwtTokenErr))
	}

	return salt, &signupData, nil
}

var ErrNoSalt = errors.New("no salt available")
var ErrNotLoggedIn = errors.New("not logged in")
var ErrInvalidCode = errors.New("invalid code")

// SignupEmailResendCode requests that the sign-up code be resent via email.
func (a *Client) SignupEmailResendCode(ctx context.Context, email string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "sign_up_email_resend_code")
	defer span.End()

	salt := a.getSaltCached()
	if salt == nil {
		return traces.RecordError(ctx, ErrNoSalt)
	}
	data := &protos.SignupEmailResendRequest{
		Email: email,
		Salt:  salt,
	}
	_, err := a.sendRequest(ctx, "POST", "/users/signup/resend/email", nil, nil, data)
	return traces.RecordError(ctx, err)
}

// SignupEmailConfirmation confirms the new account using the sign-up code received via email.
func (a *Client) SignupEmailConfirmation(ctx context.Context, email, code string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "sign_up_email_confirmation")
	defer span.End()

	data := &protos.ConfirmSignupRequest{
		Email: email,
		Code:  code,
	}
	_, err := a.sendRequest(ctx, "POST", "/users/signup/complete/email", nil, nil, data)
	return traces.RecordError(ctx, err)
}

func writeSalt(salt []byte, path string) error {
	if err := os.WriteFile(path, salt, 0600); err != nil {
		return fmt.Errorf("writing salt to %s: %w", path, err)
	}
	return nil
}

func readSalt(path string) ([]byte, error) {
	buf, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading salt from %s: %w", path, err)
	}
	if len(buf) == 0 {
		return nil, nil
	}
	return buf, nil
}

// Login logs the user in.
func (a *Client) Login(ctx context.Context, email, password string) (*UserData, error) {
	// clear any previous salt value
	a.setSalt(nil)
	ctx, span := otel.Tracer(tracerName).Start(ctx, "login")
	defer span.End()

	lowerCaseEmail := strings.ToLower(email)
	salt, err := a.getSalt(ctx, lowerCaseEmail)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}

	deviceID := settings.GetString(settings.DeviceIDKey)
	proof, err := a.clientProof(ctx, lowerCaseEmail, password, salt)
	if err != nil {
		return nil, err
	}

	loginData := &protos.LoginRequest{
		Email:    lowerCaseEmail,
		DeviceId: deviceID,
		Proof:    proof,
	}
	resp, err := a.sendRequest(ctx, "POST", "/users/login", nil, nil, loginData)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}

	var loginResp UserData
	if err := proto.Unmarshal(resp, &loginResp); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("error unmarshalling login response: %w", err))
	}
	//this can be nil if the user has reached the device limit
	if loginResp.LegacyUserData != nil {
		loginResp.LegacyUserData.DeviceID = deviceID
	}

	// regardless of state we need to save login information
	// We have device flow limit on login
	a.setData(&loginResp)
	a.setSalt(salt)
	if saltErr := writeSalt(salt, a.saltPath); saltErr != nil {
		return nil, traces.RecordError(ctx, saltErr)
	}
	return &loginResp, nil
}

// Logout logs the user out. No-op if there is no user account logged in.
func (a *Client) Logout(ctx context.Context, email string) (*UserData, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "logout")
	defer span.End()
	logout := &protos.LogoutRequest{
		Email:        email,
		DeviceId:     settings.GetString(settings.DeviceIDKey),
		LegacyUserID: settings.GetInt64(settings.UserIDKey),
		LegacyToken:  settings.GetString(settings.TokenKey),
		Token:        settings.GetString(settings.JwtTokenKey),
	}
	_, err := a.sendRequest(ctx, "POST", "/users/logout", nil, nil, logout)
	if err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("logging out: %w", err))
	}
	a.ClearUser()
	a.setSalt(nil)
	if err := writeSalt(nil, a.saltPath); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("writing salt after logout: %w", err))
	}
	return a.NewUser(ctx)
}

// StartRecoveryByEmail initializes the account recovery process for the provided email.
func (a *Client) StartRecoveryByEmail(ctx context.Context, email string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "start_recovery_by_email")
	defer span.End()

	data := &protos.StartRecoveryByEmailRequest{Email: email}
	_, err := a.sendRequest(ctx, "POST", "/users/recovery/start/email", nil, nil, data)
	return traces.RecordError(ctx, err)
}

// CompleteRecoveryByEmail completes account recovery using the code received via email.
func (a *Client) CompleteRecoveryByEmail(ctx context.Context, email, newPassword, code string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "complete_recovery_by_email")
	defer span.End()
	lowerCaseEmail := strings.ToLower(email)
	newSalt, err := generateSalt()
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	srpClient, err := newSRPClient(lowerCaseEmail, newPassword, newSalt)
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	verifierKey, err := srpClient.Verifier()
	if err != nil {
		return traces.RecordError(ctx, err)
	}

	data := &protos.CompleteRecoveryByEmailRequest{
		Email:       email,
		Code:        code,
		NewSalt:     newSalt,
		NewVerifier: verifierKey.Bytes(),
	}
	_, err = a.sendRequest(ctx, "POST", "/users/recovery/complete/email", nil, nil, data)
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to complete recovery by email: %w", err))
	}
	if err = writeSalt(newSalt, a.saltPath); err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to write new salt: %w", err))
	}
	return nil
}

// ValidateEmailRecoveryCode validates the recovery code received via email.
func (a *Client) ValidateEmailRecoveryCode(ctx context.Context, email, code string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "validate_email_recovery_code")
	defer span.End()

	data := &protos.ValidateRecoveryCodeRequest{
		Email: email,
		Code:  code,
	}
	resp, err := a.sendRequest(ctx, "POST", "/users/recovery/validate/email", nil, nil, data)
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	var codeResp protos.ValidateRecoveryCodeResponse
	if err := proto.Unmarshal(resp, &codeResp); err != nil {
		return traces.RecordError(ctx, fmt.Errorf("error unmarshalling validate recovery code response: %w", err))
	}
	if !codeResp.Valid {
		return traces.RecordError(ctx, ErrInvalidCode)
	}
	return nil
}

// StartChangeEmail initializes a change of the email address associated with this user account.
func (a *Client) StartChangeEmail(ctx context.Context, newEmail, password string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "start_change_email")
	defer span.End()

	lowerCaseEmail := strings.ToLower(settings.GetString(settings.EmailKey))
	lowerCaseNewEmail := strings.ToLower(newEmail)

	salt, err := a.getSalt(ctx, lowerCaseEmail)
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	proof, err := a.clientProof(ctx, lowerCaseEmail, password, salt)
	if err != nil {
		return traces.RecordError(ctx, err)
	}

	data := &protos.ChangeEmailRequest{
		OldEmail: lowerCaseEmail,
		NewEmail: lowerCaseNewEmail,
		Proof:    proof,
	}
	_, err = a.sendRequest(ctx, "POST", "/users/change_email", nil, nil, data)
	return traces.RecordError(ctx, err)
}

// CompleteChangeEmail completes a change of the email address associated with this user account,
// using the code received via email.
func (a *Client) CompleteChangeEmail(ctx context.Context, newEmail, password, code string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "complete_change_email")
	defer span.End()

	newSalt, err := generateSalt()
	if err != nil {
		return traces.RecordError(ctx, err)
	}

	srpClient, err := newSRPClient(newEmail, password, newSalt)
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	verifierKey, err := srpClient.Verifier()
	if err != nil {
		return traces.RecordError(ctx, err)
	}

	data := &protos.CompleteChangeEmailRequest{
		OldEmail:    settings.GetString(settings.EmailKey),
		NewEmail:    newEmail,
		Code:        code,
		NewSalt:     newSalt,
		NewVerifier: verifierKey.Bytes(),
	}
	_, err = a.sendRequest(ctx, "POST", "/users/change_email/complete/email", nil, nil, data)
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	if err := writeSalt(newSalt, a.saltPath); err != nil {
		return traces.RecordError(ctx, err)
	}
	if err := settings.Set(settings.EmailKey, newEmail); err != nil {
		return traces.RecordError(ctx, err)
	}

	a.setSalt(newSalt)
	return nil
}

// DeleteAccount deletes this user account.
func (a *Client) DeleteAccount(ctx context.Context, email, password string) (*UserData, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "delete_account")
	defer span.End()

	lowerCaseEmail := strings.ToLower(email)
	salt, err := a.getSalt(ctx, lowerCaseEmail)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	proof, err := a.clientProof(ctx, lowerCaseEmail, password, salt)
	if err != nil {
		return nil, err
	}

	data := &protos.DeleteUserRequest{
		Email:     lowerCaseEmail,
		Proof:     proof,
		Permanent: true,
		DeviceId:  settings.GetString(settings.DeviceIDKey),
	}
	_, err = a.sendRequest(ctx, "POST", "/users/delete", nil, nil, data)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}

	a.ClearUser()
	a.setSalt(nil)
	if err := writeSalt(nil, a.saltPath); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("failed to write salt during account deletion cleanup: %w", err))
	}

	return a.NewUser(ctx)
}

// OAuthLoginUrl initiates the OAuth login process for the specified provider.
func (a *Client) OAuthLoginURL(ctx context.Context, provider string) (string, error) {
	authURL := a.authURL
	if authURL == "" {
		authURL = common.GetBaseURL()
	}
	loginURL, err := url.Parse(authURL + "/users/oauth2/" + provider)
	if err != nil {
		return "", fmt.Errorf("failed to parse URL: %w", err)
	}
	query := loginURL.Query()
	query.Set("deviceId", settings.GetString(settings.DeviceIDKey))
	query.Set("userId", settings.GetString(settings.UserIDKey))
	query.Set("proToken", settings.GetString(settings.TokenKey))
	loginURL.RawQuery = query.Encode()
	return loginURL.String(), nil
}

func (a *Client) OAuthLoginCallback(ctx context.Context, oAuthToken string) (*UserData, error) {
	slog.Debug("Getting OAuth login callback")
	jwtUserInfo, err := decodeJWT(oAuthToken)
	if err != nil {
		return nil, fmt.Errorf("error decoding JWT: %w", err)
	}

	// Temporary  set user data to so api can read it
	login := &UserData{
		LegacyID:    jwtUserInfo.LegacyUserID,
		LegacyToken: jwtUserInfo.LegacyToken,
	}
	a.setData(login)
	// Get user data from api this will also save data in user config
	user, err := a.fetchUserData(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting user data: %w", err)
	}

	if err := settings.Set(settings.JwtTokenKey, oAuthToken); err != nil {
		slog.Error("Failed to persist JWT token", "error", err)
		return nil, fmt.Errorf("failed to persist JWT token: %w", err)
	}
	user.Id = jwtUserInfo.Email
	user.EmailConfirmed = true
	a.setData(user)
	return user, nil
}

type LinkResponse struct {
	*protos.BaseResponse `json:",inline"`
	UserID               int    `json:"userID"`
	ProToken             string `json:"token"`
}

// RemoveDevice removes a device from the user's account.
func (a *Client) RemoveDevice(ctx context.Context, deviceID string) (*LinkResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "remove_device")
	defer span.End()

	data := map[string]string{
		"deviceId": deviceID,
	}
	resp, err := a.sendProRequest(ctx, "POST", "/user-link-remove", nil, nil, data)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	var link LinkResponse
	if err := json.Unmarshal(resp, &link); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("error unmarshalling remove device response: %w", err))
	}
	if link.BaseResponse != nil && link.BaseResponse.Error != "" {
		return nil, traces.RecordError(ctx, fmt.Errorf("failed to remove device: %s", link.BaseResponse.Error))
	}
	return &link, nil
}

func (a *Client) ReferralAttach(ctx context.Context, code string) (bool, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "referral_attach")
	defer span.End()

	data := map[string]string{
		"code": code,
	}
	resp, err := a.sendProRequest(ctx, "POST", "/referral-attach", nil, nil, data)
	if err != nil {
		return false, traces.RecordError(ctx, err)
	}
	var baseResp protos.BaseResponse
	if err := proto.Unmarshal(resp, &baseResp); err != nil {
		return false, traces.RecordError(ctx, fmt.Errorf("error unmarshalling referral attach response: %w", err))
	}
	if baseResp.Error != "" {
		return false, traces.RecordError(ctx, errors.New(baseResp.Error))
	}
	return true, nil
}

type UserChangeEvent struct {
	events.Event
}

func (a *Client) setData(data *UserData) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if data == nil {
		a.ClearUser()
		return
	}
	if data.LegacyUserData == nil {
		slog.Info("no user data to set")
		return
	}

	existingUser := settings.GetInt64(settings.UserIDKey) != 0

	var changed bool
	if data.LegacyUserData.UserLevel != "" {
		oldUserLevel := settings.GetString(settings.UserLevelKey)
		changed = changed || oldUserLevel != data.LegacyUserData.UserLevel
		if err := settings.Set(settings.UserLevelKey, data.LegacyUserData.UserLevel); err != nil {
			slog.Error("failed to set user level in settings", "error", err)
		}
	}
	if data.LegacyUserData.Email != "" {
		oldEmail := settings.GetString(settings.EmailKey)
		changed = changed || oldEmail != data.LegacyUserData.Email
		if err := settings.Set(settings.EmailKey, data.LegacyUserData.Email); err != nil {
			slog.Error("failed to set email in settings", "error", err)
		}
	}
	if data.LegacyID != 0 {
		oldUserID := settings.GetInt64(settings.UserIDKey)
		changed = changed || oldUserID != data.LegacyID
		if err := settings.Set(settings.UserIDKey, data.LegacyID); err != nil {
			slog.Error("failed to set user ID in settings", "error", err)
		}
	}
	if data.LegacyToken != "" {
		oldToken := settings.GetString(settings.TokenKey)
		changed = changed || oldToken != data.LegacyToken
		if err := settings.Set(settings.TokenKey, data.LegacyToken); err != nil {
			slog.Error("failed to set token in settings", "error", err)
		}
	}
	if data.Token != "" {
		oldJwtToken := settings.GetString(settings.JwtTokenKey)
		changed = changed || oldJwtToken != data.Token
		if err := settings.Set(settings.JwtTokenKey, data.Token); err != nil {
			slog.Error("failed to set JWT token in settings", "error", err)
		}
	}

	devices := []settings.Device{}
	for _, d := range data.Devices {
		devices = append(devices, settings.Device{
			Name: d.Name,
			ID:   d.Id,
		})
	}
	if err := settings.Set(settings.DevicesKey, devices); err != nil {
		slog.Error("failed to set devices in settings", "error", err)
	}

	if err := settings.Set(settings.UserDataKey, data); err != nil {
		slog.Error("failed to set login response in settings", "error", err)
	}

	// We only consider the user to have changed if there was a previous user.
	if existingUser && changed {
		events.Emit(UserChangeEvent{})
	}
}

func (a *Client) ClearUser() {
	settings.Clear(settings.UserIDKey)
	settings.Clear(settings.TokenKey)
	settings.Clear(settings.UserLevelKey)
	settings.Clear(settings.EmailKey)
	settings.Clear(settings.DevicesKey)
	settings.Clear(settings.JwtTokenKey)
	settings.Clear(settings.UserDataKey)
}
