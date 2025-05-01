package api

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
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

// User represents a user account. This may be a free user, associated only with this device or a
// paid user with a full account.
type User struct {
	salt       []byte
	userData   *protos.LoginResponse
	deviceId   string
	authClient AuthClient
	userConfig common.UserInfo
}

// NewUser returns the object handling anything user-auth related
// It takes a httpClient and a userConfig object.
func NewUser(httpClient *http.Client, userConfig common.UserInfo) *User {
	salt, _ := userConfig.ReadSalt()
	userData, _ := userConfig.GetUserData()
	opts := common.WebClientOptions{
		HttpClient: httpClient,
		BaseURL:    common.APIBaseUrl,
	}

	return &User{
		authClient: &authClient{common.NewWebClient(&opts)},
		salt:       salt,
		userData:   userData,
		deviceId:   userConfig.DeviceID(),
		userConfig: userConfig,
	}
}

// Devices returns a list of devices associated with this user account.
func (u *User) Devices() ([]Device, error) {
	if u.userData == nil {
		return nil, ErrNotLoggedIn
	}
	ret := []Device{}
	for _, d := range u.userData.Devices {
		ret = append(ret, Device{
			Name: d.Name,
			ID:   d.Id,
		})
	}

	return ret, nil
}

// Subscription returns the subscription status of this user account.
func (u *User) Subscription() (Subscription, error) {
	// TODO: implement me!
	return Subscription{}, common.ErrNotImplemented
}

// DataCapInfo returns information about this user's data cap. Only valid for free accounts.
func (u *User) DataCapInfo() (*DataCapInfo, error) {
	// TODO: implement me!
	return nil, common.ErrNotImplemented
}

// SignUp signs the user up for an account.
func (u *User) SignUp(ctx context.Context, email, password string) error {
	salt, err := u.authClient.SignUp(ctx, email, password)
	if err == nil {
		u.salt = salt
		return u.userConfig.WriteSalt(salt)
	}

	return err
}

var ErrNoSalt = errors.New("not salt available, call GetSalt/Signup first")
var ErrNotLoggedIn = errors.New("not logged in")
var ErrInvalidCode = errors.New("invalid code")

// SignUpEmailResendCode requests that the sign-up code be resent via email.
func (u *User) SignupEmailResendCode(ctx context.Context, email string) error {
	if u.salt == nil {
		return ErrNoSalt
	}
	return u.authClient.SignupEmailResendCode(ctx, &protos.SignupEmailResendRequest{
		Email: email,
		Salt:  u.salt,
	})
}

// SignupEmailConfirmation confirms the new account using the sign-up code received via email.
func (u *User) SignupEmailConfirmation(ctx context.Context, email, code string) error {
	return u.authClient.SignupEmailConfirmation(ctx, &protos.ConfirmSignupRequest{
		Email: email,
		Code:  code,
	})
}

// getSalt retrieves the salt for the given email address or it's cached value.
func (u *User) getSalt(ctx context.Context, email string) ([]byte, error) {
	if u.salt != nil {
		return u.salt, nil // use cached value
	}
	resp, err := u.authClient.GetSalt(ctx, email)
	if err != nil {
		return nil, ErrNoSalt
	}
	u.salt = resp.Salt
	if err := u.userConfig.WriteSalt(resp.Salt); err != nil {
		return nil, err
	}
	return resp.Salt, nil
}

// Login logs the user in.
func (u *User) Login(ctx context.Context, email string, password string, deviceId string) error {
	salt, err := u.getSalt(ctx, email)
	if err != nil {
		return err
	}
	resp, err := u.authClient.Login(ctx, email, password, deviceId, salt)
	if err == nil {
		u.userConfig.Save(resp)
		u.userData = resp
	}
	return err
}

// Logout logs the user out. No-op if there is no user account logged in.
func (u *User) Logout(ctx context.Context) error {
	return u.authClient.SignOut(ctx, &protos.LogoutRequest{
		Email:        u.userData.Id,
		DeviceId:     u.deviceId,
		LegacyUserID: u.userData.LegacyID,
		LegacyToken:  u.userData.LegacyToken,
	})
}

// StartRecoveryByEmail initializes the account recovery process for the provided email.
func (u *User) StartRecoveryByEmail(ctx context.Context, email string) error {
	return u.authClient.StartRecoveryByEmail(ctx, &protos.StartRecoveryByEmailRequest{
		Email: email,
	})
}

// CompleteRecoveryByEmail completes account recovery using the code received via email.
func (u *User) CompleteRecoveryByEmail(ctx context.Context, email, newPassword, code string) error {
	lowerCaseEmail := strings.ToLower(email)
	newSalt, err := GenerateSalt()
	if err != nil {
		return err
	}
	srpClient, err := NewSRPClient(lowerCaseEmail, newPassword, newSalt)
	if err != nil {
		return err
	}
	verifierKey, err := srpClient.Verifier()
	if err != nil {
		return err
	}

	err = u.authClient.CompleteRecoveryByEmail(ctx, &protos.CompleteRecoveryByEmailRequest{
		Email:       email,
		Code:        code,
		NewSalt:     newSalt,
		NewVerifier: verifierKey.Bytes(),
	})
	if err == nil {
		err = u.userConfig.WriteSalt(newSalt)
	}
	return err
}

// ValidateEmailRecoveryCode validates the recovery code received via email.
func (u *User) ValidateEmailRecoveryCode(ctx context.Context, email, code string) error {
	resp, err := u.authClient.ValidateEmailRecoveryCode(ctx, &protos.ValidateRecoveryCodeRequest{
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
func (u *User) StartChangeEmail(ctx context.Context, newEmail string, password string) error {
	if u.userData == nil {
		return ErrNotLoggedIn
	}
	lowerCaseEmail := strings.ToLower(u.userData.Id)
	lowerCaseNewEmail := strings.ToLower(newEmail)
	salt, err := u.getSalt(ctx, lowerCaseEmail)
	if err != nil {
		return err
	}

	// Prepare login request body
	encKey, err := GenerateEncryptedKey(password, lowerCaseEmail, salt)
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
	srpB, err := u.authClient.LoginPrepare(ctx, prepareRequestBody)
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

	return u.authClient.ChangeEmail(ctx, changeEmailRequestBody)
}

// CompleteChangeEmail completes a change of the email address associated with this user account,
// using the code recieved via email.
func (u *User) CompleteChangeEmail(ctx context.Context, newEmail, password, code string) error {
	newSalt, err := GenerateSalt()
	if err != nil {
		return err
	}

	encKey, err := GenerateEncryptedKey(password, newEmail, newSalt)
	if err != nil {
		return err
	}

	srpClient := srp.NewSRPClient(srp.KnownGroups[group], encKey, nil)
	verifierKey, err := srpClient.Verifier()
	if err != nil {
		return err
	}

	if err := u.authClient.CompleteChangeEmail(ctx, &protos.CompleteChangeEmailRequest{
		OldEmail:    u.userData.Id,
		NewEmail:    newEmail,
		Code:        code,
		NewSalt:     newSalt,
		NewVerifier: verifierKey.Bytes(),
	}); err != nil {
		return err
	}
	if err := u.userConfig.WriteSalt(newSalt); err != nil {
		return err
	}

	if err := u.userConfig.Save(u.userData); err != nil {
		return err
	}

	u.salt = newSalt
	u.userData.Id = newEmail
	return nil
}

// DeleteAccount deletes this user account.
func (u *User) DeleteAccount(ctx context.Context, password string) error {
	if u.userData == nil {
		return ErrNotLoggedIn
	}
	lowerCaseEmail := strings.ToLower(u.userData.Id)
	salt, err := u.getSalt(ctx, lowerCaseEmail)
	if err != nil {
		return err
	}

	// Prepare login request body
	encKey, err := GenerateEncryptedKey(password, lowerCaseEmail, salt)
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

	srpB, err := u.authClient.LoginPrepare(ctx, prepareRequestBody)
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
		DeviceId:  u.deviceId,
	}

	if err := u.authClient.DeleteAccount(ctx, changeEmailRequestBody); err != nil {
		return err
	}

	u.userData = nil
	return u.userConfig.Save(nil)
}

// OAuthLogin initiates the OAuth login process for the specified provider.
func (u *User) OAuthLoginUrl(ctx context.Context, provider string) (*protos.SubscriptionPaymentRedirectResponse, error) {
	loginUrl, err := url.Parse(fmt.Sprintf("%s/%s/%s", "https://df.iantem.io/api/v1", "users/oauth2", provider))
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}
	query := loginUrl.Query()
	query.Set("deviceId", u.deviceId)
	query.Set("userId", strconv.FormatInt(u.userConfig.LegacyID(), 10))
	query.Set("proToken", u.userData.LegacyToken)
	loginUrl.RawQuery = query.Encode()

	return &protos.SubscriptionPaymentRedirectResponse{
		Redirect: loginUrl.String(),
	}, nil
}
