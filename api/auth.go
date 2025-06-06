package api

import (
	"context"
	"fmt"
	"strconv"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
)

type AuthClient interface {
	// Sign up methods
	SignUp(ctx context.Context, email string, password string) ([]byte, error)
	SignupEmailResendCode(ctx context.Context, data *protos.SignupEmailResendRequest) error
	SignupEmailConfirmation(ctx context.Context, data *protos.ConfirmSignupRequest) error
	// Login methods
	GetSalt(ctx context.Context, email string) (*protos.GetSaltResponse, error)
	LoginPrepare(ctx context.Context, loginData *protos.PrepareRequest) (*protos.PrepareResponse, error)
	Login(ctx context.Context, email, password, deviceID string, salt []byte) (*protos.LoginResponse, error)
	// Recovery methods
	StartRecoveryByEmail(ctx context.Context, loginData *protos.StartRecoveryByEmailRequest) error
	CompleteRecoveryByEmail(ctx context.Context, loginData *protos.CompleteRecoveryByEmailRequest) error
	ValidateEmailRecoveryCode(ctx context.Context, loginData *protos.ValidateRecoveryCodeRequest) (*protos.ValidateRecoveryCodeResponse, error)
	// Change email methods
	ChangeEmail(ctx context.Context, loginData *protos.ChangeEmailRequest) error
	// Complete change email methods
	CompleteChangeEmail(ctx context.Context, loginData *protos.CompleteChangeEmailRequest) error
	DeleteAccount(ctc context.Context, loginData *protos.DeleteUserRequest) error
	// Logout
	SignOut(ctx context.Context, logoutData *protos.LogoutRequest) error
}

type authClient struct {
	wc       *webClient
	userIndo common.UserInfo
}

// Auth APIS
// GetSalt is used to get the salt for a given email address
func (c *authClient) GetSalt(ctx context.Context, email string) (*protos.GetSaltResponse, error) {
	var resp protos.GetSaltResponse
	query := map[string]string{
		"email": email,
	}
	header := map[string]string{
		"Content-Type": "application/x-protobuf",
		"Accept":       "application/x-protobuf",
	}
	req := c.wc.NewRequest(query, header, nil)
	if err := c.wc.Get(ctx, "/users/salt", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Sign up API
// SignUp is used to sign up a new user with the SignupRequest
func (c *authClient) signUp(ctx context.Context, signupData *protos.SignupRequest) error {
	var resp protos.EmptyResponse
	header := map[string]string{
		backend.DeviceIDHeader: c.userIndo.DeviceID(),
		backend.UserIDHeader:   strconv.FormatInt(c.userIndo.LegacyID(), 10),
		backend.ProTokenHeader: c.userIndo.LegacyToken(),
	}
	req := c.wc.NewRequest(nil, header, signupData)
	return c.wc.Post(ctx, "/users/signup", req, &resp)
}

// SignupEmailResendCode is used to resend the email confirmation code
// Params: ctx context.Context, data *SignupEmailResendRequest
func (c *authClient) SignupEmailResendCode(ctx context.Context, data *protos.SignupEmailResendRequest) error {
	var resp protos.EmptyResponse
	req := c.wc.NewRequest(nil, nil, data)
	return c.wc.Post(ctx, "/users/signup/resend/email", req, &resp)
}

// SignupEmailConfirmation is used to confirm the email address once user enter code
// Params: ctx context.Context, data *ConfirmSignupRequest
func (c *authClient) SignupEmailConfirmation(ctx context.Context, data *protos.ConfirmSignupRequest) error {
	var resp protos.EmptyResponse
	req := c.wc.NewRequest(nil, nil, data)
	return c.wc.Post(ctx, "/users/signup/complete/email", req, &resp)
}

// LoginPrepare does the initial login preparation with come make sure the user exists and match user salt
func (c *authClient) LoginPrepare(ctx context.Context, loginData *protos.PrepareRequest) (*protos.PrepareResponse, error) {
	var model protos.PrepareResponse
	req := c.wc.NewRequest(nil, nil, loginData)
	if err := c.wc.Post(ctx, "/users/prepare", req, &model); err != nil {
		// Send custom error to show error on client side
		return nil, fmt.Errorf("user_not_found %w", err)
	}
	return &model, nil
}

// Login is used to login a user with the LoginRequest
func (c *authClient) login(ctx context.Context, loginData *protos.LoginRequest) (*protos.LoginResponse, error) {
	var resp protos.LoginResponse
	req := c.wc.NewRequest(nil, nil, loginData)
	if err := c.wc.Post(ctx, "/users/login", req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// StartRecoveryByEmail is used to start the recovery process by sending a recovery code to the user's email
func (c *authClient) StartRecoveryByEmail(ctx context.Context, loginData *protos.StartRecoveryByEmailRequest) error {
	var resp protos.EmptyResponse
	req := c.wc.NewRequest(nil, nil, loginData)
	return c.wc.Post(ctx, "/users/recovery/start/email", req, &resp)
}

// CompleteRecoveryByEmail is used to complete the recovery process by validating the recovery code
func (c *authClient) CompleteRecoveryByEmail(ctx context.Context, loginData *protos.CompleteRecoveryByEmailRequest) error {
	var resp protos.EmptyResponse
	req := c.wc.NewRequest(nil, nil, loginData)
	return c.wc.Post(ctx, "/users/recovery/complete/email", req, &resp)
}

// // ValidateEmailRecoveryCode is used to validate the recovery code
func (c *authClient) ValidateEmailRecoveryCode(ctx context.Context, recoveryData *protos.ValidateRecoveryCodeRequest) (*protos.ValidateRecoveryCodeResponse, error) {
	var resp protos.ValidateRecoveryCodeResponse
	req := c.wc.NewRequest(nil, nil, recoveryData)
	err := c.wc.Post(ctx, "/users/recovery/validate/email", req, &resp)
	if err != nil {
		return nil, err
	}
	if !resp.Valid {
		return nil, fmt.Errorf("invalid_code Error decoding response body: %w", err)
	}
	return &resp, nil
}

// ChangeEmail is used to change the email address of a user
func (c *authClient) ChangeEmail(ctx context.Context, loginData *protos.ChangeEmailRequest) error {
	var resp protos.EmptyResponse
	req := c.wc.NewRequest(nil, nil, loginData)
	return c.wc.Post(ctx, "/users/change_email", req, &resp)
}

// CompleteChangeEmail is used to complete the email change process
func (c *authClient) CompleteChangeEmail(ctx context.Context, loginData *protos.CompleteChangeEmailRequest) error {
	var resp protos.EmptyResponse
	req := c.wc.NewRequest(nil, nil, loginData)
	return c.wc.Post(ctx, "/users/change_email/complete/email", req, &resp)
}

// DeleteAccount is used to delete the account of a user
// Once account is delete make sure to create new account
func (c *authClient) DeleteAccount(ctx context.Context, accountData *protos.DeleteUserRequest) error {
	var resp protos.EmptyResponse
	req := c.wc.NewRequest(nil, nil, accountData)
	return c.wc.Post(ctx, "/users/delete", req, &resp)
}

// DeleteAccount is used to delete the account of a user
// Once account is delete make sure to create new account
func (c *authClient) SignOut(ctx context.Context, logoutData *protos.LogoutRequest) error {
	var resp protos.EmptyResponse
	req := c.wc.NewRequest(nil, nil, logoutData)
	return c.wc.Post(ctx, "/users/logout", req, &resp)
}
