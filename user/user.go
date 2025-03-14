package user

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/1Password/srp"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/user/deviceid"
	"google.golang.org/protobuf/proto"
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

var (
	saltLocation     = ".salt" // TODO: we need to think about properly storing data. Right now both configFetcher and this module just dump things in the current directory. Instead there should be a 'data writer' that knows where to put things.
	userDataLocation = ".userData"
)

// User represents a user account. This may be a free user, associated only with this device or a
// paid user with a full account.
type User struct {
	salt       []byte
	userData   *LoginResponse
	deviceId   string
	authClient AuthClient
}

func (u *User) DeviceID() string {
	return u.deviceId
}

func (u *User) LegacyID() int64 {
	if u.userData == nil {
		return 0
	}
	return u.userData.LegacyID
}

func (u *User) LegacyToken() string {
	if u.userData == nil {
		return ""
	}
	return u.userData.LegacyToken
}

func writeUserData(data *LoginResponse) error {
	return os.WriteFile(userDataLocation, []byte(data.String()), 0600)

}

func readUserData() (*LoginResponse, error) {
	data, err := os.ReadFile(userDataLocation)
	if err != nil {
		return nil, err
	}
	var resp LoginResponse
	if err := proto.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func writeSalt(salt []byte) error {
	return os.WriteFile(saltLocation, salt, 0600)
}

func readSalt() ([]byte, error) {
	return os.ReadFile(saltLocation)
}

// New returns the object handling anything user-account related
func New(httpClient *http.Client) *User {
	salt, _ := readSalt()
	userData, _ := readUserData()
	return &User{
		authClient: &authClient{common.NewWebClient(httpClient)},
		salt:       salt,
		userData:   userData,
		deviceId:   deviceid.Get(),
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
		return writeSalt(salt)
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
	return u.authClient.SignupEmailResendCode(ctx, &SignupEmailResendRequest{
		Email: email,
		Salt:  u.salt,
	})
}

// SignupEmailConfirmation confirms the new account using the sign-up code received via email.
func (u *User) SignupEmailConfirmation(ctx context.Context, email, code string) error {
	return u.authClient.SignupEmailConfirmation(ctx, &ConfirmSignupRequest{
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
	if err := writeSalt(resp.Salt); err != nil {
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
		writeUserData(resp)
		u.userData = resp
	}
	return err
}

// Logout logs the user out. No-op if there is no user account logged in.
func (u *User) Logout(ctx context.Context) error {
	return u.authClient.SignOut(ctx, &LogoutRequest{
		Email:        u.userData.Id,
		DeviceId:     u.deviceId,
		LegacyUserID: u.userData.LegacyID,
		LegacyToken:  u.userData.LegacyToken,
	})
}

// StartRecoveryByEmail initializes the account recovery process for the provided email.
func (u *User) StartRecoveryByEmail(ctx context.Context, email string) error {
	return u.authClient.StartRecoveryByEmail(ctx, &StartRecoveryByEmailRequest{
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

	err = u.authClient.CompleteRecoveryByEmail(ctx, &CompleteRecoveryByEmailRequest{
		Email:       email,
		Code:        code,
		NewSalt:     newSalt,
		NewVerifier: verifierKey.Bytes(),
	})
	if err == nil {
		err = writeSalt(newSalt)
	}
	return err
}

// ValidateEmailRecoveryCode validates the recovery code received via email.
func (u *User) ValidateEmailRecoveryCode(ctx context.Context, email, code string) error {
	resp, err := u.authClient.ValidateEmailRecoveryCode(ctx, &ValidateRecoveryCodeRequest{
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
	prepareRequestBody := &PrepareRequest{
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

	changeEmailRequestBody := &ChangeEmailRequest{
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

	if err := u.authClient.CompleteChangeEmail(ctx, &CompleteChangeEmailRequest{
		OldEmail:    u.userData.Id,
		NewEmail:    newEmail,
		Code:        code,
		NewSalt:     newSalt,
		NewVerifier: verifierKey.Bytes(),
	}); err != nil {
		return err
	}
	if err := writeSalt(newSalt); err != nil {
		return err
	}

	if err := writeUserData(u.userData); err != nil {
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
	prepareRequestBody := &PrepareRequest{
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

	changeEmailRequestBody := &DeleteUserRequest{
		Email:     lowerCaseEmail,
		Proof:     clientProof,
		Permanent: true,
		DeviceId:  u.deviceId,
	}

	if err := u.authClient.DeleteAccount(ctx, changeEmailRequestBody); err != nil {
		return err
	}

	u.userData = nil
	return writeUserData(nil)
}
