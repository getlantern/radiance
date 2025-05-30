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

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
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

// Subscription holds information about a user's paid subscription.
type Subscription struct {
	Tier    Tier
	Expires time.Time
}

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

// Create a new user account
func (ac *APIClient) NewUser(ctx context.Context) (*UserDataResponse, error) {
	var resp UserDataResponse
	err := ac.proWC.Post(ctx, "/user-create", nil, &resp)
	if err != nil {
		slog.Error("creating new user", "error", err)
		return nil, err
	}
	if resp.LoginResponse_UserData == nil {
		slog.Error("creating new user", "error", "no user data in response")
		return nil, fmt.Errorf("no user data in response")
	}
	login := &protos.LoginResponse{
		LegacyID:       resp.UserId,
		LegacyToken:    resp.Token,
		LegacyUserData: resp.LoginResponse_UserData,
	}
	err = ac.userInfo.SetData(login)
	if err != nil {
		slog.Error("setting user data", "error", err)
		return nil, err
	}
	return &resp, nil
}

// UserData returns the user data
func (ac *APIClient) UserData(ctx context.Context) (*UserDataResponse, error) {
	var resp UserDataResponse
	err := ac.proWC.Get(ctx, "/user-data", nil, &resp)
	if err != nil {
		slog.Error("user data", "error", err)
		return nil, fmt.Errorf("getting user data: %w", err)
	}
	if resp.BaseResponse != nil && resp.Error != "" {
		err = fmt.Errorf("recevied bad response: %s", resp.Error)
		slog.Error("user data", "error", err)
		return nil, err
	}
	if resp.LoginResponse_UserData == nil {
		slog.Error("user data", "error", "no user data in response")
		return nil, fmt.Errorf("no user data in response")
	}
	login := &protos.LoginResponse{
		LegacyID:       resp.UserId,
		LegacyToken:    resp.Token,
		LegacyUserData: resp.LoginResponse_UserData,
	}
	err = ac.userInfo.SetData(login)
	if err != nil {
		slog.Error("setting user data", "error", err)
		return nil, err
	}
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

// TODO: do we want to store the subscription status in the user config?
//			or should we just always request it from the server when needed?

// Subscription returns the subscription status of this user account.
func (a *APIClient) Subscription() (Subscription, error) {
	// TODO: implement me!
	return Subscription{}, common.ErrNotImplemented
}

// DataCapInfo returns information about this user's data cap. Only valid for free accounts.
func (a *APIClient) DataCapInfo() (*DataCapInfo, error) {
	// TODO: implement me!
	return nil, common.ErrNotImplemented
}

// SignUp signs the user up for an account.
func (a *APIClient) SignUp(ctx context.Context, email, password string) error {
	salt, err := a.authClient.SignUp(ctx, email, password)
	if err == nil {
		a.salt = salt
		return writeSalt(salt, a.saltPath)
	}

	return err
}

var ErrNoSalt = errors.New("not salt available, call GetSalt/Signup first")
var ErrNotLoggedIn = errors.New("not logged in")
var ErrInvalidCode = errors.New("invalid code")

// SignUpEmailResendCode requests that the sign-up code be resent via email.
func (a *APIClient) SignupEmailResendCode(ctx context.Context, email string) error {
	if a.salt == nil {
		return ErrNoSalt
	}
	return a.authClient.SignupEmailResendCode(ctx, &protos.SignupEmailResendRequest{
		Email: email,
		Salt:  a.salt,
	})
}

// SignupEmailConfirmation confirms the new account using the sign-up code received via email.
func (a *APIClient) SignupEmailConfirmation(ctx context.Context, email, code string) error {
	return a.authClient.SignupEmailConfirmation(ctx, &protos.ConfirmSignupRequest{
		Email: email,
		Code:  code,
	})
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
	return buf, nil
}

// getSalt retrieves the salt for the given email address or it's cached value.
func (a *APIClient) getSalt(ctx context.Context, email string) ([]byte, error) {
	if a.salt != nil {
		return a.salt, nil // use cached value
	}
	resp, err := a.authClient.GetSalt(ctx, email)
	if err != nil {
		return nil, err
	}
	a.salt = resp.Salt
	if err := writeSalt(resp.Salt, a.saltPath); err != nil {
		return nil, err
	}
	return resp.Salt, nil
}

// Login logs the user in.
func (a *APIClient) Login(ctx context.Context, email string, password string, deviceId string) (*protos.LoginResponse, error) {
	salt, err := a.getSalt(ctx, email)
	if err != nil {
		return nil, err
	}
	resp, err := a.authClient.Login(ctx, email, password, deviceId, salt)
	if err != nil {
		return nil, err
	}
	// If login was successful, update the user info and cache the user data.
	// We have device flow limit on login
	if resp.Success {
		a.userInfo.SetData(resp)
		a.userData = resp
	}
	return resp, nil
}

// Logout logs the user out. No-op if there is no user account logged in.
func (a *APIClient) Logout(ctx context.Context, email string) error {
	return a.authClient.SignOut(ctx, &protos.LogoutRequest{
		Email:        email,
		DeviceId:     a.userInfo.DeviceID(),
		LegacyUserID: a.userInfo.LegacyID(),
		LegacyToken:  a.userInfo.LegacyToken(),
	})
}

// StartRecoveryByEmail initializes the account recovery process for the provided email.
func (a *APIClient) StartRecoveryByEmail(ctx context.Context, email string) error {
	return a.authClient.StartRecoveryByEmail(ctx, &protos.StartRecoveryByEmailRequest{
		Email: email,
	})
}

// CompleteRecoveryByEmail completes account recovery using the code received via email.
func (a *APIClient) CompleteRecoveryByEmail(ctx context.Context, email, newPassword, code string) error {
	lowerCaseEmail := strings.ToLower(email)
	newSalt, err := generateSalt()
	if err != nil {
		return err
	}
	srpClient, err := newSRPClient(lowerCaseEmail, newPassword, newSalt)
	if err != nil {
		return err
	}
	verifierKey, err := srpClient.Verifier()
	if err != nil {
		return err
	}

	err = a.authClient.CompleteRecoveryByEmail(ctx, &protos.CompleteRecoveryByEmailRequest{
		Email:       email,
		Code:        code,
		NewSalt:     newSalt,
		NewVerifier: verifierKey.Bytes(),
	})
	if err != nil {
		return fmt.Errorf("failed to complete recovery by email: %w", err)
	}
	if err = writeSalt(newSalt, a.saltPath); err != nil {
		return fmt.Errorf("failed to write new salt: %w", err)
	}
	return nil
}

// ValidateEmailRecoveryCode validates the recovery code received via email.
func (a *APIClient) ValidateEmailRecoveryCode(ctx context.Context, email, code string) error {
	resp, err := a.authClient.ValidateEmailRecoveryCode(ctx, &protos.ValidateRecoveryCodeRequest{
		Email: email,
		Code:  code,
	})
	if err != nil {
		return err
	}
	if !resp.Valid {
		return ErrInvalidCode
	}
	return nil
}

const group = srp.RFC5054Group3072

// StartChangeEmail initializes a change of the email address associated with this user account.
func (a *APIClient) StartChangeEmail(ctx context.Context, newEmail string, password string) error {
	if a.userData == nil {
		return ErrNotLoggedIn
	}
	lowerCaseEmail := strings.ToLower(a.userData.Id)
	lowerCaseNewEmail := strings.ToLower(newEmail)
	salt, err := a.getSalt(ctx, lowerCaseEmail)
	if err != nil {
		return err
	}

	// Prepare login request body
	encKey, err := generateEncryptedKey(password, lowerCaseEmail, salt)
	if err != nil {
		return err
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
		return err
	}
	// Once the client receives B from the server Client should check error status here as defense against
	// a malicious B sent from server
	B := big.NewInt(0).SetBytes(srpB.B)

	if err = client.SetOthersPublic(B); err != nil {
		return err
	}

	// client can now make the session key
	clientKey, err := client.Key()
	if err != nil || clientKey == nil {
		return fmt.Errorf("user_not_found error while generating Client key %w", err)
	}

	// // check if the server proof is valid
	if !client.GoodServerProof(salt, lowerCaseEmail, srpB.Proof) {
		return fmt.Errorf("user_not_found error while checking server proof %w", err)
	}

	clientProof, err := client.ClientProof()
	if err != nil {
		return fmt.Errorf("user_not_found error while generating client proof %w", err)
	}

	changeEmailRequestBody := &protos.ChangeEmailRequest{
		OldEmail: lowerCaseEmail,
		NewEmail: lowerCaseNewEmail,
		Proof:    clientProof,
	}

	return a.authClient.ChangeEmail(ctx, changeEmailRequestBody)
}

// CompleteChangeEmail completes a change of the email address associated with this user account,
// using the code recieved via email.
func (a *APIClient) CompleteChangeEmail(ctx context.Context, newEmail, password, code string) error {
	newSalt, err := generateSalt()
	if err != nil {
		return err
	}

	encKey, err := generateEncryptedKey(password, newEmail, newSalt)
	if err != nil {
		return err
	}

	srpClient := srp.NewSRPClient(srp.KnownGroups[group], encKey, nil)
	verifierKey, err := srpClient.Verifier()
	if err != nil {
		return err
	}

	if err := a.authClient.CompleteChangeEmail(ctx, &protos.CompleteChangeEmailRequest{
		OldEmail:    a.userData.Id,
		NewEmail:    newEmail,
		Code:        code,
		NewSalt:     newSalt,
		NewVerifier: verifierKey.Bytes(),
	}); err != nil {
		return err
	}
	if err := writeSalt(newSalt, a.saltPath); err != nil {
		return err
	}

	if err := a.userInfo.SetData(a.userData); err != nil {
		return err
	}

	a.salt = newSalt
	a.userData.Id = newEmail
	return nil
}

// DeleteAccount deletes this user account.
func (a *APIClient) DeleteAccount(ctx context.Context, password string) error {
	if a.userData == nil {
		return ErrNotLoggedIn
	}
	lowerCaseEmail := strings.ToLower(a.userData.Id)
	salt, err := a.getSalt(ctx, lowerCaseEmail)
	if err != nil {
		return err
	}

	// Prepare login request body
	encKey, err := generateEncryptedKey(password, lowerCaseEmail, salt)
	if err != nil {
		return err
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
		return err
	}

	B := big.NewInt(0).SetBytes(srpB.B)

	if err = client.SetOthersPublic(B); err != nil {

		return err
	}

	clientKey, err := client.Key()
	if err != nil || clientKey == nil {
		return fmt.Errorf("user_not_found error while generating Client key %w", err)
	}

	// // check if the server proof is valid
	if !client.GoodServerProof(salt, lowerCaseEmail, srpB.Proof) {
		return fmt.Errorf("user_not_found error while checking server proof %w", err)
	}

	clientProof, err := client.ClientProof()
	if err != nil {
		return fmt.Errorf("user_not_found error while generating client proof %w", err)
	}

	changeEmailRequestBody := &protos.DeleteUserRequest{
		Email:     lowerCaseEmail,
		Proof:     clientProof,
		Permanent: true,
		DeviceId:  a.deviceID,
	}

	if err := a.authClient.DeleteAccount(ctx, changeEmailRequestBody); err != nil {
		return err
	}

	a.userData = nil
	return a.userInfo.SetData(nil)
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
