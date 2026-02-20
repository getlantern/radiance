package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/1Password/srp"

	"github.com/r3labs/sse/v2"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/traces"
)

// The main output of this file is Radiance.GetUser, which provides a hook into all user account
// functionality.

const saltFileName = ".salt"

// pro-server requests
type UserDataResponse struct {
	*protos.BaseResponse           `json:",inline"`
	*protos.LoginResponse_UserData `json:",inline"`
}

// NewUser creates a new user account
func (ac *APIClient) NewUser(ctx context.Context) (*protos.LoginResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "new_user")
	defer span.End()

	var resp UserDataResponse
	header := map[string]string{
		backend.ContentTypeHeader: "application/json",
	}
	req := ac.proWebClient().NewRequest(nil, header, nil)
	err := ac.proWebClient().Post(ctx, "/user-create", req, &resp)
	if err != nil {
		slog.Error("creating new user", "error", err)
		return nil, traces.RecordError(ctx, err)
	}
	loginResponse, err := ac.storeData(ctx, resp)
	if err != nil {
		return nil, err
	}
	return loginResponse, nil
}

func (ac *APIClient) UserData() ([]byte, error) {
	slog.Debug("Getting user data")
	user := &protos.LoginResponse{}
	err := settings.GetStruct(settings.LoginResponseKey, user)
	return withMarshalProto(user, err)
}

// FetchUserData fetches user data from the server.
func (ac *APIClient) FetchUserData(ctx context.Context) ([]byte, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "fetch_user_data")
	defer span.End()
	return withMarshalProto(ac.fetchUserData(ctx))
}

// fetchUserData calls the /user-data endpoint and stores the result via storeData.
func (ac *APIClient) fetchUserData(ctx context.Context) (*protos.LoginResponse, error) {
	var resp UserDataResponse
	err := ac.proWebClient().Get(ctx, "/user-data", nil, &resp)
	if err != nil {
		slog.Error("user data", "error", err)
		return nil, traces.RecordError(ctx, fmt.Errorf("getting user data: %w", err))
	}
	return ac.storeData(ctx, resp)
}

func (a *APIClient) storeData(ctx context.Context, resp UserDataResponse) (*protos.LoginResponse, error) {
	if resp.BaseResponse != nil && resp.Error != "" {
		err := fmt.Errorf("received bad response: %s", resp.Error)
		slog.Error("user data", "error", err)
		return nil, traces.RecordError(ctx, err)
	}
	if resp.LoginResponse_UserData == nil {
		slog.Error("user data", "error", "no user data in response")
		return nil, traces.RecordError(ctx, fmt.Errorf("no user data in response"))
	}
	// Append device ID to user data
	resp.LoginResponse_UserData.DeviceID = settings.GetString(settings.DeviceIDKey)
	login := &protos.LoginResponse{
		LegacyID:       resp.UserId,
		LegacyToken:    resp.Token,
		LegacyUserData: resp.LoginResponse_UserData,
	}
	a.setData(login)
	return login, nil
}

// user-server requests

// Devices returns a list of devices associated with this user account.
func (a *APIClient) Devices() ([]settings.Device, error) {
	return settings.Devices()
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
func (a *APIClient) DataCapInfo(ctx context.Context) (string, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "data_cap_info")
	defer span.End()
	datacap := &DataCapUsageResponse{}
	headers := map[string]string{
		backend.ContentTypeHeader: "application/json",
	}
	getURL := fmt.Sprintf("/datacap/%s", settings.GetString(settings.DeviceIDKey))
	authWc := authWebClient()
	newReq := authWc.NewRequest(nil, headers, nil)
	err := authWc.Get(ctx, getURL, newReq, &datacap)
	return withMarshalJsonString(datacap, err)
}

type DataCapChangeEvent struct {
	events.Event
	*DataCapUsageResponse
}

// DataCapStream connects to the datacap SSE endpoint and continuously reads events.
// It sends events whenever there is an update in datacap usage with DataCapChangeEvent.
// To receive those events use events.Subscribe(&DataCapChangeEvent{}, func(evt DataCapChangeEvent) { ... })
func (a *APIClient) DataCapStream(ctx context.Context) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "data_cap_info_stream")
	defer span.End()

	getURL := fmt.Sprintf("/stream/datacap/%s", settings.GetString(settings.DeviceIDKey))
	authWc := authWebClient()
	fullURL := common.GetBaseURL() + getURL
	sseClient := sse.NewClient(fullURL)
	sseClient.Headers = map[string]string{
		backend.ContentTypeHeader: "application/json",
		backend.AcceptHeader:      "text/event-stream",
		backend.AppNameHeader:     common.Name,
		backend.VersionHeader:     common.Version,
		backend.PlatformHeader:    common.Platform,
	}
	sseClient.Connection.Transport = authWc.client.GetClient().Transport
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
func (a *APIClient) SignUp(ctx context.Context, email, password string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "sign_up")
	defer span.End()

	salt, err := a.authClient.SignUp(ctx, email, password)
	if err == nil {
		a.salt = salt
		return traces.RecordError(ctx, writeSalt(salt, a.saltPath))
	}
	return traces.RecordError(ctx, err)
}

var ErrNoSalt = errors.New("not salt available, call GetSalt/Signup first")
var ErrNotLoggedIn = errors.New("not logged in")
var ErrInvalidCode = errors.New("invalid code")

// SignupEmailResendCode requests that the sign-up code be resent via email.
func (a *APIClient) SignupEmailResendCode(ctx context.Context, email string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "sign_up_email_resend_code")
	defer span.End()

	if a.salt == nil {
		return traces.RecordError(ctx, ErrNoSalt)
	}
	return traces.RecordError(ctx, a.authClient.SignupEmailResendCode(ctx, &protos.SignupEmailResendRequest{
		Email: email,
		Salt:  a.salt,
	}))
}

// SignupEmailConfirmation confirms the new account using the sign-up code received via email.
func (a *APIClient) SignupEmailConfirmation(ctx context.Context, email, code string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "sign_up_email_confirmation")
	defer span.End()

	return traces.RecordError(ctx, a.authClient.SignupEmailConfirmation(ctx, &protos.ConfirmSignupRequest{
		Email: email,
		Code:  code,
	}))
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

// getSalt retrieves the salt for the given email address or it's cached value.
func (a *APIClient) getSalt(ctx context.Context, email string) ([]byte, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "get_salt")
	defer span.End()

	if a.salt != nil {
		return a.salt, nil // use cached value
	}
	resp, err := a.authClient.GetSalt(ctx, email)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	return resp.Salt, nil
}

// Login logs the user in.
func (a *APIClient) Login(ctx context.Context, email string, password string) ([]byte, error) {
	// clear any previous salt value
	a.salt = nil
	ctx, span := otel.Tracer(tracerName).Start(ctx, "login")
	defer span.End()

	salt, err := a.getSalt(ctx, email)
	if err != nil {
		return nil, err
	}

	deviceId := settings.GetString(settings.DeviceIDKey)
	resp, err := a.authClient.Login(ctx, email, password, deviceId, salt)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}

	//this can be nil if the user has reached the device limit
	if resp.LegacyUserData != nil {
		// Append device ID to user data
		resp.LegacyUserData.DeviceID = deviceId
	}

	// regardless of state we need to save login information
	// We have device flow limit on login
	a.setData(resp)
	a.salt = salt
	if saltErr := writeSalt(salt, a.saltPath); saltErr != nil {
		return nil, traces.RecordError(ctx, saltErr)
	}
	return withMarshalProto(resp, nil)
}

// Logout logs the user out. No-op if there is no user account logged in.
func (a *APIClient) Logout(ctx context.Context, email string) ([]byte, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "logout")
	defer span.End()
	if err := a.authClient.SignOut(ctx, &protos.LogoutRequest{
		Email:        email,
		DeviceId:     settings.GetString(settings.DeviceIDKey),
		LegacyUserID: settings.GetInt64(settings.UserIDKey),
		LegacyToken:  settings.GetString(settings.TokenKey),
	}); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("logging out: %w", err))
	}
	a.Reset()
	a.salt = nil
	if err := writeSalt(nil, a.saltPath); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("writing salt after logout: %w", err))
	}
	return withMarshalProto(a.NewUser(context.Background()))
}

func withMarshalProto(resp *protos.LoginResponse, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	protoUserData, err := proto.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("error marshalling login response: %w", err)
	}
	return protoUserData, nil
}

func withMarshalJson(data any, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("error marshalling user data: %w", err)
	}
	return jsonData, nil
}

func withMarshalJsonString(data any, err error) (string, error) {
	raw, err := withMarshalJson(data, err)
	return string(raw), err
}

// StartRecoveryByEmail initializes the account recovery process for the provided email.
func (a *APIClient) StartRecoveryByEmail(ctx context.Context, email string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "start_recovery_by_email")
	defer span.End()

	return traces.RecordError(ctx, a.authClient.StartRecoveryByEmail(ctx, &protos.StartRecoveryByEmailRequest{
		Email: email,
	}))
}

// CompleteRecoveryByEmail completes account recovery using the code received via email.
func (a *APIClient) CompleteRecoveryByEmail(ctx context.Context, email, newPassword, code string) error {
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

	err = a.authClient.CompleteRecoveryByEmail(ctx, &protos.CompleteRecoveryByEmailRequest{
		Email:       email,
		Code:        code,
		NewSalt:     newSalt,
		NewVerifier: verifierKey.Bytes(),
	})
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to complete recovery by email: %w", err))
	}
	if err = writeSalt(newSalt, a.saltPath); err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to write new salt: %w", err))
	}
	return nil
}

// ValidateEmailRecoveryCode validates the recovery code received via email.
func (a *APIClient) ValidateEmailRecoveryCode(ctx context.Context, email, code string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "validate_email_recovery_code")
	defer span.End()

	resp, err := a.authClient.ValidateEmailRecoveryCode(ctx, &protos.ValidateRecoveryCodeRequest{
		Email: email,
		Code:  code,
	})
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	if !resp.Valid {
		return traces.RecordError(ctx, ErrInvalidCode)
	}
	return nil
}

const group = srp.RFC5054Group3072

// StartChangeEmail initializes a change of the email address associated with this user account.
func (a *APIClient) StartChangeEmail(ctx context.Context, newEmail string, password string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "start_change_email")
	defer span.End()
	lowerCaseEmail := strings.ToLower(settings.GetString(settings.EmailKey))
	lowerCaseNewEmail := strings.ToLower(newEmail)
	salt, err := a.getSalt(ctx, lowerCaseEmail)
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	// Prepare login request body
	encKey, err := generateEncryptedKey(password, lowerCaseEmail, salt)
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	client := srp.NewSRPClient(srp.KnownGroups[group], encKey, nil)

	//Send this key to client
	A := client.EphemeralPublic()

	//Create body
	prepareRequestBody := &protos.PrepareRequest{
		Email: lowerCaseEmail,
		A:     A.Bytes(),
	}

	srpB, err := a.authClient.LoginPrepare(ctx, prepareRequestBody)
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	// Once the client receives B from the server Client should check error status here as defense against
	// a malicious B sent from server
	B := big.NewInt(0).SetBytes(srpB.B)

	if err = client.SetOthersPublic(B); err != nil {
		return traces.RecordError(ctx, err)
	}

	// client can now make the session key
	clientKey, err := client.Key()
	if err != nil || clientKey == nil {
		return traces.RecordError(ctx, fmt.Errorf("user_not_found error while generating Client key %w", err))
	}

	// // check if the server proof is valid
	if !client.GoodServerProof(salt, lowerCaseEmail, srpB.Proof) {
		return traces.RecordError(ctx, fmt.Errorf("user_not_found error while checking server proof %w", err))
	}

	clientProof, err := client.ClientProof()
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("user_not_found error while generating client proof %w", err))
	}

	changeEmailRequestBody := &protos.ChangeEmailRequest{
		OldEmail: lowerCaseEmail,
		NewEmail: lowerCaseNewEmail,
		Proof:    clientProof,
	}

	return traces.RecordError(ctx, a.authClient.ChangeEmail(ctx, changeEmailRequestBody))
}

// CompleteChangeEmail completes a change of the email address associated with this user account,
// using the code recieved via email.
func (a *APIClient) CompleteChangeEmail(ctx context.Context, newEmail, password, code string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "complete_change_email")
	defer span.End()
	newSalt, err := generateSalt()
	if err != nil {
		return traces.RecordError(ctx, err)
	}

	encKey, err := generateEncryptedKey(password, newEmail, newSalt)
	if err != nil {
		return traces.RecordError(ctx, err)
	}

	srpClient := srp.NewSRPClient(srp.KnownGroups[group], encKey, nil)
	verifierKey, err := srpClient.Verifier()
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	if err := a.authClient.CompleteChangeEmail(ctx, &protos.CompleteChangeEmailRequest{
		OldEmail:    settings.GetString(settings.EmailKey),
		NewEmail:    newEmail,
		Code:        code,
		NewSalt:     newSalt,
		NewVerifier: verifierKey.Bytes(),
	}); err != nil {
		return traces.RecordError(ctx, err)
	}
	if err := writeSalt(newSalt, a.saltPath); err != nil {
		return traces.RecordError(ctx, err)
	}
	if err := settings.Set(settings.EmailKey, newEmail); err != nil {
		return traces.RecordError(ctx, err)
	}

	a.salt = newSalt
	return nil
}

// DeleteAccount deletes this user account.
func (a *APIClient) DeleteAccount(ctx context.Context, email, password string) ([]byte, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "delete_account")
	defer span.End()
	lowerCaseEmail := strings.ToLower(email)
	salt, err := a.getSalt(ctx, lowerCaseEmail)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}

	// Prepare login request body
	encKey, err := generateEncryptedKey(password, lowerCaseEmail, salt)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	client := srp.NewSRPClient(srp.KnownGroups[group], encKey, nil)

	//Send this key to client
	A := client.EphemeralPublic()

	//Create body
	prepareRequestBody := &protos.PrepareRequest{
		Email: lowerCaseEmail,
		A:     A.Bytes(),
	}

	srpB, err := a.authClient.LoginPrepare(ctx, prepareRequestBody)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}

	B := big.NewInt(0).SetBytes(srpB.B)

	if err = client.SetOthersPublic(B); err != nil {
		return nil, traces.RecordError(ctx, err)
	}

	clientKey, err := client.Key()
	if err != nil || clientKey == nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("user_not_found error while generating Client key %w", err))
	}

	// // check if the server proof is valid
	if !client.GoodServerProof(salt, lowerCaseEmail, srpB.Proof) {
		return nil, traces.RecordError(ctx, fmt.Errorf("user_not_found error while checking server proof %w", err))
	}

	clientProof, err := client.ClientProof()
	if err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("user_not_found error while generating client proof %w", err))
	}

	changeEmailRequestBody := &protos.DeleteUserRequest{
		Email:     lowerCaseEmail,
		Proof:     clientProof,
		Permanent: true,
		DeviceId:  settings.GetString(settings.DeviceIDKey),
	}

	if err := a.authClient.DeleteAccount(ctx, changeEmailRequestBody); err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	// clean up local data
	a.Reset()
	a.salt = nil
	if err := writeSalt(nil, a.saltPath); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("failed to write salt during account deletion cleanup: %w", err))
	}

	return withMarshalProto(a.NewUser(context.Background()))
}

// OAuthLoginUrl initiates the OAuth login process for the specified provider.
func (a *APIClient) OAuthLoginUrl(ctx context.Context, provider string) (string, error) {
	loginURL, err := url.Parse(fmt.Sprintf("%s/%s/%s", common.GetBaseURL(), "users/oauth2", provider))
	if err != nil {
		return "", fmt.Errorf("failed to parse URL: %w", err)
	}
	query := loginURL.Query()
	query.Set("deviceId", settings.GetString(settings.DeviceIDKey))
	query.Set("userId", strconv.FormatInt(settings.GetInt64(settings.UserIDKey), 10))
	query.Set("proToken", settings.GetString(settings.TokenKey))
	loginURL.RawQuery = query.Encode()
	return loginURL.String(), nil
}

func (a *APIClient) OAuthLoginCallback(ctx context.Context, oAuthToken string) ([]byte, error) {
	slog.Debug("Getting OAuth login callback")
	jwtUserInfo, err := decodeJWT(oAuthToken)
	if err != nil {
		return nil, fmt.Errorf("error decoding JWT: %w", err)
	}

	// Temporary  set user data to so api can read it
	login := &protos.LoginResponse{
		LegacyID:    jwtUserInfo.LegacyUserID,
		LegacyToken: jwtUserInfo.LegacyToken,
	}
	a.setData(login)
	// Get user data from api this will also save data in user config
	user, err := a.fetchUserData(context.Background())
	if err != nil {
		return nil, fmt.Errorf("error getting user data: %w", err)
	}
	user.Id = jwtUserInfo.Email
	user.EmailConfirmed = true
	a.setData(user)
	return withMarshalProto(user, nil)
}

type LinkResponse struct {
	*protos.BaseResponse `json:",inline"`
	UserID               int    `json:"userID"`
	ProToken             string `json:"token"`
}

// RemoveDevice removes a device from the user's account.
func (a *APIClient) RemoveDevice(ctx context.Context, deviceID string) (*LinkResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "remove_device")
	defer span.End()

	data := map[string]string{
		"deviceId": deviceID,
	}
	proWC := a.proWebClient()
	req := proWC.NewRequest(nil, nil, data)
	resp := &LinkResponse{}
	if err := proWC.Post(ctx, "/user-link-remove", req, resp); err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	if resp.BaseResponse != nil && resp.BaseResponse.Error != "" {
		return nil, traces.RecordError(ctx, fmt.Errorf("failed to remove device: %s", resp.BaseResponse.Error))
	}
	return resp, nil
}

func (a *APIClient) ReferralAttach(ctx context.Context, code string) (bool, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "referral_attach")
	defer span.End()

	data := map[string]string{
		"code": code,
	}
	proWC := a.proWebClient()
	req := proWC.NewRequest(nil, nil, data)
	resp := &protos.BaseResponse{}
	if err := proWC.Post(ctx, "/referral-attach", req, resp); err != nil {
		return false, traces.RecordError(ctx, err)
	}
	if resp.Error != "" {
		return false, traces.RecordError(ctx, fmt.Errorf("%s", resp.Error))
	}
	return true, nil
}

func (a *APIClient) setData(data *protos.LoginResponse) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if data == nil {
		a.Reset()
		return
	}
	var changed bool
	if data.LegacyUserData == nil {
		slog.Info("no user data to set")
		return
	}

	existingUser := settings.GetInt64(settings.UserIDKey) != 0

	if data.LegacyUserData.UserLevel != "" {
		oldUserLevel := settings.GetString(settings.UserLevelKey)
		changed = changed || oldUserLevel != data.LegacyUserData.UserLevel
		if err := settings.Set(settings.UserLevelKey, data.LegacyUserData.UserLevel); err != nil {
			slog.Error("failed to set user level in settings", "error", err)
		}
	}
	if data.LegacyUserData.Email != "" {
		oldEmail := settings.GetString(settings.EmailKey)
		changed = changed && oldEmail != data.LegacyUserData.Email
		if err := settings.Set(settings.EmailKey, data.LegacyUserData.Email); err != nil {
			slog.Error("failed to set email in settings", "error", err)
		}
	}
	if data.LegacyID != 0 {
		oldUserID := settings.GetInt64(settings.UserIDKey)
		changed = changed && oldUserID != data.LegacyID
		if err := settings.Set(settings.UserIDKey, data.LegacyID); err != nil {
			slog.Error("failed to set user ID in settings", "error", err)
		}
	}
	if data.LegacyToken != "" {
		oldToken := settings.GetString(settings.TokenKey)
		changed = changed && oldToken != data.LegacyToken
		if err := settings.Set(settings.TokenKey, data.LegacyToken); err != nil {
			slog.Error("failed to set token in settings", "error", err)
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

	if err := settings.Set(settings.LoginResponseKey, data); err != nil {
		slog.Error("failed to set login response in settings", "error", err)
	}

	// We only consider the user to have changed if there was a previous user.
	if existingUser && changed {
		events.Emit(settings.UserChangeEvent{})
	}
}

func (a *APIClient) Reset() {
	// Clear user data
	settings.Set(settings.UserIDKey, int64(0))
	settings.Set(settings.TokenKey, "")
	settings.Set(settings.UserLevelKey, "")
	settings.Set(settings.EmailKey, "")
	settings.Set(settings.DevicesKey, []settings.Device{})
}
