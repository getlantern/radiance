package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/1Password/srp"
	"github.com/getlantern/common"
	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/backend"
	rcommon "github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/traces"
	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/otel"
)

const tracerName = "github.com/getlantern/radiance/api"

type APIClient struct {
	authWc *webClient
	proWC  *webClient

	salt          []byte
	saltPath      string
	deviceID      string
	authClient    AuthClient
	userInfo      rcommon.UserInfo
	configHandler *config.ConfigHandler
}

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

func NewAPIClient(httpClient *http.Client, userInfo rcommon.UserInfo, dataDir string, configHandler *config.ConfigHandler) *APIClient {
	httpClient.Transport = traces.NewRoundTripper(httpClient.Transport)
	path := filepath.Join(dataDir, saltFileName)
	salt, err := readSalt(path)
	if err != nil {
		slog.Warn("failed to read salt", "error", err)
	}

	proWC := newWebClient(httpClient, proServerURL)
	proWC.client.OnBeforeRequest(func(client *resty.Client, req *resty.Request) error {
		req.Header.Set(backend.DeviceIDHeader, userInfo.DeviceID())
		if userInfo.Token() != "" {
			req.Header.Set(backend.ProTokenHeader, userInfo.Token())
		}
		if userInfo.ID() != 0 {
			req.Header.Set(backend.UserIDHeader, strconv.FormatInt(userInfo.ID(), 10))
		}
		return nil
	})
	wc := newWebClient(httpClient, baseURL)
	return &APIClient{
		authWc:        wc,
		proWC:         proWC,
		salt:          salt,
		saltPath:      path,
		deviceID:      userInfo.DeviceID(),
		authClient:    &authClient{wc, userInfo},
		userInfo:      userInfo,
		configHandler: configHandler,
	}
}

// FetchUserData refreshes and returns the user data from the server for the currently logged in user.
// If there is no logged in user, this creates a new one.
func (ac *APIClient) FetchUserData(ctx context.Context) (*common.UserData, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "user_data")
	defer span.End()

	ac.configHandler.RefreshConfig()
	return ac.userInfo.GetData()
}

// user-server requests

// Devices returns a list of devices associated with this user account.
func (a *APIClient) Devices() ([]common.Device, error) {
	data, err := a.userInfo.GetData()
	if err != nil {
		return nil, ErrNotLoggedIn
	}
	ret := []common.Device{}
	for _, d := range data.Devices {
		ret = append(ret, common.Device{
			Name: d.Name,
			Id:   d.Id,
		})
	}

	return ret, nil
}

// DataCapInfo returns information about this user's data cap
func (a *APIClient) DataCapInfo(ctx context.Context) (*DataCapInfo, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "data_cap_info")
	defer span.End()
	datacap := &DataCapInfo{}
	getUrl := fmt.Sprintf("/datacap/user/%d/device/%s/usage", a.userInfo.ID(), a.userInfo.DeviceID())
	newReq := a.authWc.NewRequest(nil, nil, nil)
	err := a.authWc.Get(ctx, getUrl, newReq, &datacap)
	if err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	return datacap, nil
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

	//this can be nil if the user has reached the device limit
	if resp.LegacyUserData != nil {
		// Append device ID to user data
		resp.LegacyUserData.DeviceID = deviceId
	}

	// regardless of state we need to save login information
	// We have device flow limit on login
	a.userInfo.SetData(protoToJson(resp))
	a.salt = salt
	if err := writeSalt(salt, a.saltPath); err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	return resp, nil
}

// Logout logs the user out. No-op if there is no user account logged in.
func (a *APIClient) Logout(ctx context.Context, email string) ([]byte, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "logout")
	defer span.End()
	err := a.authClient.SignOut(ctx, &protos.LogoutRequest{
		Email:        email,
		DeviceId:     a.userInfo.DeviceID(),
		LegacyUserID: a.userInfo.ID(),
		LegacyToken:  a.userInfo.Token(),
	})
	if err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("logging out: %w", err))
	}
	// clean up local data
	a.userInfo.SetData(nil)
	a.salt = nil
	if err := writeSalt(nil, a.saltPath); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("writing salt after logout: %w", err))
	}
	return a.resetUser()
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
	data, err := a.userInfo.GetData()
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("No user data %v", err))
	}
	if data == nil {
		return traces.RecordError(ctx, ErrNotLoggedIn)
	}
	lowerCaseEmail := strings.ToLower(data.Email)
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

	data, err := a.userInfo.GetData()
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("No user data %v", err))
	}
	if data == nil {
		return traces.RecordError(ctx, ErrNotLoggedIn)
	}
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
		OldEmail:    data.Email,
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
	data.Email = newEmail

	if err := a.userInfo.SetData(data); err != nil {
		return traces.RecordError(ctx, err)
	}
	a.salt = newSalt
	return nil
}

func protoToJson(data *protos.LoginResponse) *common.UserData {
	if data == nil {
		return nil
	}
	return &common.UserData{
		UserId:           data.LegacyUserData.UserId,
		Code:             data.LegacyUserData.Code,
		Token:            data.LegacyUserData.Token,
		Referral:         data.LegacyUserData.Referral,
		Email:            data.LegacyUserData.Email,
		Status:           data.LegacyUserData.UserStatus,
		Level:            common.UserLevel(data.LegacyUserData.UserLevel),
		Locale:           data.LegacyUserData.Locale,
		Expiration:       data.LegacyUserData.Expiration,
		Subscription:     data.LegacyUserData.Subscription,
		BonusDays:        data.LegacyUserData.BonusDays,
		BonusMonths:      data.LegacyUserData.BonusMonths,
		Inviters:         data.LegacyUserData.Inviters,
		Invitees:         data.LegacyUserData.Invitees,
		Devices:          toCommonDevices(data.LegacyUserData.Devices),
		SubscriptionData: toCommonSubscriptionData(data.LegacyUserData.SubscriptionData),
	}
}

func toCommonDevices(devices []*protos.LoginResponse_Device) []common.Device {
	if devices == nil {
		return nil
	}
	ret := make([]common.Device, 0, len(devices))
	for _, d := range devices {
		ret = append(ret, common.Device{
			Name:    d.Name,
			Id:      d.Id,
			Created: d.Created,
		})
	}
	return ret
}

func toCommonSubscriptionData(data *protos.LoginResponse_UserData_SubscriptionData) *common.SubscriptionData {
	if data == nil {
		return nil
	}
	createdAtString, err := time.Parse(time.RFC3339, data.CreatedAt)
	if err != nil {
		createdAtString = time.Unix(0, 0)
	}
	startAtString, err := time.Parse(time.RFC3339, data.StartAt)
	if err != nil {
		startAtString = time.Unix(0, 0)
	}
	endAtString, err := time.Parse(time.RFC3339, data.EndAt)
	if err != nil {
		endAtString = time.Unix(0, 0)
	}
	var cancelledAtTime *time.Time
	if data.CancelledAt != "" {
		t, err := time.Parse(time.RFC3339, data.CancelledAt)
		if err == nil {
			cancelledAtTime = &t
		}
	}
	return &common.SubscriptionData{
		SubscriptionID:   data.SubscriptionID,
		PlanID:           data.PlanID,
		StripeCustomerID: data.StripeCustomerID,
		Status:           data.Status,
		Provider:         data.Provider,
		CreatedAt:        createdAtString,
		StartAt:          startAtString,
		EndAt:            endAtString,
		CancelledAt:      cancelledAtTime,
		AutoRenew:        data.AutoRenew,
	}
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
		DeviceId:  a.deviceID,
	}

	if err := a.authClient.DeleteAccount(ctx, changeEmailRequestBody); err != nil {
		return nil, traces.RecordError(ctx, err)
	}
	// clean up local data
	a.userInfo.SetData(nil)
	a.salt = nil
	if err := writeSalt(nil, a.saltPath); err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("failed to write salt during account deletion cleanup: %w", err))
	}
	err = traces.RecordError(ctx, a.userInfo.SetData(nil))

	// We always want to have a current user.
	rawJson, err := a.resetUser()
	if err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("error resetting user after account deletion: %w", err))
	}
	return rawJson, nil
}

// OAuthLogin initiates the OAuth login process for the specified provider.
func (a *APIClient) OAuthLoginUrl(ctx context.Context, provider string) (string, error) {
	loginURL, err := url.Parse(fmt.Sprintf("%s/%s/%s", "https://df.iantem.io/api/v1", "users/oauth2", provider))
	if err != nil {
		return "", fmt.Errorf("failed to parse URL: %w", err)
	}
	query := loginURL.Query()
	query.Set("deviceId", a.userInfo.DeviceID())
	query.Set("userId", strconv.FormatInt(a.userInfo.ID(), 10))
	query.Set("proToken", a.userInfo.Token())
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

func (a *APIClient) resetUser() ([]byte, error) {
	user, err := a.FetchUserData(context.Background())
	if err != nil {
		return nil, fmt.Errorf("error creating user: %w", err)
	}
	jsonUserData, err := json.Marshal(user)
	if err != nil {
		return nil, fmt.Errorf("error marshalling user data: %w", err)
	}
	a.userInfo.SetData(user)
	return jsonUserData, nil
}
