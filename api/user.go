package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/1Password/srp"
	"go.opentelemetry.io/otel"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/traces"
)

// The main output of this file is Radiance.GetUser, which provides a hook into all user account
// functionality.

// DataCapInfo represents information about the data cap for a user account.
type DataCapInfo struct {
	BytesAllotted, BytesRemaining int
	AllotmentStart, AllotmentEnd  time.Time
}

// Tier is the level of subscription a user is currently at.
type Tier int

const (
	TierFree = 0
	TierPro  = 1

	saltFileName = ".salt"

	baseURL = "https://iantem.io/api/v1"
)

// Device is a machine registered to a user account (e.g. an Android phone or a Windows desktop).
type Device struct {
	ID   string
	Name string
}

// pro-server requests

type UserDataResponse struct {
	*protos.BaseResponse           `json:",inline"`
	*protos.LoginResponse_UserData `json:",inline"`
}

// NewUser creates a new user account
func (ac *APIClient) NewUser(ctx context.Context) (*UserDataResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "new_user")
	defer span.End()
	var resp UserDataResponse
	err := ac.proWC.Post(ctx, "/user-create", nil, &resp)
	if err != nil {
		slog.Error("creating new user", "error", err)
		return nil, traces.RecordError(ctx, err)
	}
	if resp.LoginResponse_UserData == nil {
		slog.Error("creating new user", "error", "no user data in response")
		return nil, traces.RecordError(ctx, fmt.Errorf("no user data in response"))
	}
	// Append device ID to user data
	resp.LoginResponse_UserData.DeviceID = ac.userInfo.DeviceID()
	login := &protos.LoginResponse{
		LegacyID:       resp.UserId,
		LegacyToken:    resp.Token,
		LegacyUserData: resp.LoginResponse_UserData,
	}
	err = ac.userInfo.SetData(login)
	if err != nil {
		slog.Error("setting user data", "error", err)
		return nil, traces.RecordError(ctx, err)
	}
	// update the user data
	ac.userData = login
	return &resp, nil
}

// UserData returns the user data
func (ac *APIClient) UserData(ctx context.Context) (*UserDataResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "user_data")
	defer span.End()
	var resp UserDataResponse
	err := ac.proWC.Get(ctx, "/user-data", nil, &resp)
	if err != nil {
		slog.Error("user data", "error", err)
		return nil, traces.RecordError(ctx, fmt.Errorf("getting user data: %w", err))
	}
	if resp.BaseResponse != nil && resp.Error != "" {
		err = fmt.Errorf("recevied bad response: %s", resp.Error)
		slog.Error("user data", "error", err)
		return nil, traces.RecordError(ctx, err)
	}
	if resp.LoginResponse_UserData == nil {
		slog.Error("user data", "error", "no user data in response")
		return nil, traces.RecordError(ctx, fmt.Errorf("no user data in response"))
	}
	// Append device ID to user data
	resp.LoginResponse_UserData.DeviceID = ac.userInfo.DeviceID()
	login := &protos.LoginResponse{
		LegacyID:       resp.UserId,
		LegacyToken:    resp.Token,
		LegacyUserData: resp.LoginResponse_UserData,
	}
	err = ac.userInfo.SetData(login)
	if err != nil {
		slog.Error("setting user data", "error", err)
		return nil, traces.RecordError(ctx, err)
	}
	// update the user data
	ac.userData = login
	return &resp, nil
}

// user-server requests

// Devices returns a list of devices associated with this user account.
func (a *APIClient) Devices() ([]Device, error) {
	if a.userData == nil {
		return nil, ErrNotLoggedIn
	}
	ret := []Device{}
	for _, d := range a.userData.Devices {
		ret = append(ret, Device{
			Name: d.Name,
			ID:   d.Id,
		})
	}

	return ret, nil
}

// DataCapInfo returns information about this user's data cap
func (a *APIClient) DataCapInfo() (*DataCapInfo, error) {
	// TODO: implement me!
	return nil, common.ErrNotImplemented
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

// SignUpEmailResendCode requests that the sign-up code be resent via email.
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
func (a *APIClient) Login(ctx context.Context, email string, password string, deviceId string) (*protos.LoginResponse, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "login")
	defer span.End()
	salt, err := a.getSalt(ctx, email)
	if err != nil {
		return nil, err
	}
	resp, err := a.authClient.Login(ctx, email, password, deviceId, salt)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	// Append device ID to user data
	resp.LegacyUserData.DeviceID = deviceId

	// regardless of state we need to save login information
	// We have device flow limit on login
	a.userInfo.SetData(resp)
	a.userData = resp
	a.salt = salt
	if err := writeSalt(salt, a.saltPath); err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	return resp, nil
}

// Logout logs the user out. No-op if there is no user account logged in.
func (a *APIClient) Logout(ctx context.Context, email string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "logout")
	defer span.End()
	err := a.authClient.SignOut(ctx, &protos.LogoutRequest{
		Email:        email,
		DeviceId:     a.userInfo.DeviceID(),
		LegacyUserID: a.userInfo.LegacyID(),
		LegacyToken:  a.userInfo.LegacyToken(),
	})
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("logging out: %w", err))
	}
	// clean up local data
	a.userData = nil
	a.salt = nil
	if err := writeSalt(nil, a.saltPath); err != nil {
		return traces.RecordError(ctx, fmt.Errorf("writing salt after logout: %w", err))
	}
	return nil
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
	if a.userData == nil {
		return traces.RecordError(ctx, ErrNotLoggedIn)
	}
	lowerCaseEmail := strings.ToLower(a.userData.LegacyUserData.Email)
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
		OldEmail:    a.userData.LegacyUserData.Email,
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

	if err := a.userInfo.SetData(a.userData); err != nil {
		return traces.RecordError(ctx, err)
	}
	a.salt = newSalt
	a.userData.LegacyUserData.Email = newEmail
	return nil
}

// DeleteAccount deletes this user account.
func (a *APIClient) DeleteAccount(ctx context.Context, email, password string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "delete_account")
	defer span.End()
	lowerCaseEmail := strings.ToLower(email)
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

	B := big.NewInt(0).SetBytes(srpB.B)

	if err = client.SetOthersPublic(B); err != nil {
		return traces.RecordError(ctx, err)
	}

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

	changeEmailRequestBody := &protos.DeleteUserRequest{
		Email:     lowerCaseEmail,
		Proof:     clientProof,
		Permanent: true,
		DeviceId:  a.deviceID,
	}

	if err := a.authClient.DeleteAccount(ctx, changeEmailRequestBody); err != nil {
		return traces.RecordError(ctx, err)
	}
	// clean up local data
	a.userData = nil
	a.salt = nil
	if err := writeSalt(nil, a.saltPath); err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to write salt during account deletion cleanup: %w", err))
	}
	return traces.RecordError(ctx, a.userInfo.SetData(nil))
}

// OAuthLogin initiates the OAuth login process for the specified provider.
func (a *APIClient) OAuthLoginUrl(ctx context.Context, provider string) (string, error) {
	loginURL, err := url.Parse(fmt.Sprintf("%s/%s/%s", "https://df.iantem.io/api/v1", "users/oauth2", provider))
	if err != nil {
		return "", fmt.Errorf("failed to parse URL: %w", err)
	}
	query := loginURL.Query()
	query.Set("deviceId", a.userInfo.DeviceID())
	query.Set("userId", strconv.FormatInt(a.userInfo.LegacyID(), 10))
	query.Set("proToken", a.userInfo.LegacyToken())
	loginURL.RawQuery = query.Encode()
	return loginURL.String(), nil
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
	req := a.proWC.NewRequest(nil, nil, data)
	resp := &LinkResponse{}
	if err := a.proWC.Post(ctx, "/user-link-remove", req, resp); err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	if resp.BaseResponse != nil && resp.BaseResponse.Error != "" {
		return nil, traces.RecordError(ctx, fmt.Errorf("failed to remove device: %s", resp.BaseResponse.Error))
	}
	return resp, nil
}
