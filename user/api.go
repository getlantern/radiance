package user

import (
	"context"
	"fmt"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/user/protos"
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
	common.WebClient
}

// Auth APIS
// GetSalt is used to get the salt for a given email address
func (c *authClient) GetSalt(ctx context.Context, email string) (*protos.GetSaltResponse, error) {
	var resp protos.GetSaltResponse
	err := c.GetPROTOC(ctx, "/users/salt", map[string]interface{}{
		"email": email,
	}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// Sign up API
// SignUp is used to sign up a new user with the SignupRequest
func (c *authClient) signUp(ctx context.Context, signupData *protos.SignupRequest) error {
	var resp protos.EmptyResponse
	return c.PostPROTOC(ctx, "/users/signup", signupData, &resp)
}

// SignupEmailResendCode is used to resend the email confirmation code
// Params: ctx context.Context, data *SignupEmailResendRequest
func (c *authClient) SignupEmailResendCode(ctx context.Context, data *protos.SignupEmailResendRequest) error {
	var resp protos.EmptyResponse
	return c.PostPROTOC(ctx, "/users/signup/resend/email", data, &resp)
}

// SignupEmailConfirmation is used to confirm the email address once user enter code
// Params: ctx context.Context, data *ConfirmSignupRequest
func (c *authClient) SignupEmailConfirmation(ctx context.Context, data *protos.ConfirmSignupRequest) error {
	var resp protos.EmptyResponse
	return c.PostPROTOC(ctx, "/users/signup/complete/email", data, &resp)
}

// LoginPrepare does the initial login preparation with come make sure the user exists and match user salt
func (c *authClient) LoginPrepare(ctx context.Context, loginData *protos.PrepareRequest) (*protos.PrepareResponse, error) {
	var model protos.PrepareResponse
	err := c.PostPROTOC(ctx, "/users/prepare", loginData, &model)
	if err != nil {
		// Send custom error to show error on client side
		return nil, fmt.Errorf("user_not_found %w", err)
	}
	return &model, nil
}

// Login is used to login a user with the LoginRequest
func (c *authClient) login(ctx context.Context, loginData *protos.LoginRequest) (*protos.LoginResponse, error) {
	var resp protos.LoginResponse
	err := c.PostPROTOC(ctx, "/users/login", loginData, &resp)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// StartRecoveryByEmail is used to start the recovery process by sending a recovery code to the user's email
func (c *authClient) StartRecoveryByEmail(ctx context.Context, loginData *protos.StartRecoveryByEmailRequest) error {
	var resp protos.EmptyResponse
	return c.PostPROTOC(ctx, "/users/recovery/start/email", loginData, &resp)
}

// CompleteRecoveryByEmail is used to complete the recovery process by validating the recovery code
func (c *authClient) CompleteRecoveryByEmail(ctx context.Context, loginData *protos.CompleteRecoveryByEmailRequest) error {
	var resp protos.EmptyResponse
	return c.PostPROTOC(ctx, "/users/recovery/complete/email", loginData, &resp)
}

// // ValidateEmailRecoveryCode is used to validate the recovery code
func (c *authClient) ValidateEmailRecoveryCode(ctx context.Context, recoveryData *protos.ValidateRecoveryCodeRequest) (*protos.ValidateRecoveryCodeResponse, error) {
	var resp protos.ValidateRecoveryCodeResponse
	err := c.PostPROTOC(ctx, "/users/recovery/validate/email", recoveryData, &resp)
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
	return c.PostPROTOC(ctx, "/users/change_email", loginData, &resp)
}

// CompleteChangeEmail is used to complete the email change process
func (c *authClient) CompleteChangeEmail(ctx context.Context, loginData *protos.CompleteChangeEmailRequest) error {
	var resp protos.EmptyResponse
	return c.PostPROTOC(ctx, "/users/change_email/complete/email", loginData, &resp)
}

// DeleteAccount is used to delete the account of a user
// Once account is delete make sure to create new account
func (c *authClient) DeleteAccount(ctx context.Context, accountData *protos.DeleteUserRequest) error {
	var resp protos.EmptyResponse
	return c.PostPROTOC(ctx, "/users/delete", accountData, &resp)
}

// DeleteAccount is used to delete the account of a user
// Once account is delete make sure to create new account
func (c *authClient) SignOut(ctx context.Context, logoutData *protos.LogoutRequest) error {
	var resp protos.EmptyResponse
	return c.PostPROTOC(ctx, "/users/logout", logoutData, &resp)
}
