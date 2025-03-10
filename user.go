package radiance

import (
	"context"
	"time"
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
	Name     string
	Platform string
}

// User represents a user account. This may be a free user, associated only with this device or a
// paid user with a full account.
type User struct {
}

// GetUser returns information about the current user.
func (r *Radiance) GetUser() (*User, error) {
	// TODO: implement me!
	return nil, ErrNotImplemented
}

// Devices returns a list of devices associated with this user account.
func (u *User) Devices() []Device {
	// TODO: implement me!
	return []Device{}
}

// Subscription returns the subscription status of this user account.
func (u *User) Subscription() Subscription {
	// TODO: implement me!
	return Subscription{}
}

// DataCapInfo returns information about this user's data cap. Only valid for free accounts.
func (u *User) DataCapInfo() (*DataCapInfo, error) {
	// TODO: implement me!
	return nil, ErrNotImplemented
}

// SignUp signs the user up for an account.
func (u *User) SignUp(ctx context.Context, email, password string) error {
	// TODO: implement me!
	return ErrNotImplemented
}

// SignUpEmailResendCode requests that the sign-up code be resent via email.
func (u *User) SignupEmailResendCode(ctx context.Context, email string) error {
	// TODO: implement me!
	return ErrNotImplemented
}

// SignupEmailConfirmation confirms the new account using the sign-up code received via email.
func (u *User) SignupEmailConfirmation(ctx context.Context, email, code string) error {
	// TODO: implement me!
	return ErrNotImplemented
}

// Login logs the user in.
func (u *User) Login(ctx context.Context, email string, password string, deviceId string) error {
	// TODO: implement me!
	return ErrNotImplemented
}

// Logout logs the user out. No-op if there is no user account logged in.
func (u *User) Logout(ctx context.Context) error {
	// TODO: implement me!
	return ErrNotImplemented
}

// StartRecoveryByEmail initializes the account recovery process for the provided email.
func (u *User) StartRecoveryByEmail(ctx context.Context, email string) error {
	// TODO: implement me!
	return ErrNotImplemented
}

// CompleteRecoveryByEmail completes account recovery using the code received via email.
func (u *User) CompleteRecoveryByEmail(ctx context.Context, email, code string) error {
	// TODO: implement me!
	return ErrNotImplemented
}

// ValidateEmailRecoveryCode validates the recovery code received via email.
func (u *User) ValidateEmailRecoveryCode(ctx context.Context, email, code string) error {
	// TODO: implement me!
	return ErrNotImplemented
}

// StartChangeEmail initializes a change of the email address associated with this user account.
func (u *User) StartChangeEmail(ctx context.Context, newEmail string) error {
	// TODO: implement me!
	return ErrNotImplemented
}

// CompleteChangeEmail completes a change of the email address associated with this user account,
// using the code recieved via email.
func (u *User) CompleteChangeEmail(ctx context.Context, newEmail, code string) error {
	// TODO: implement me!
	return ErrNotImplemented
}

// DeleteAccount deletes this user account.
func (u *User) DeleteAccount(ctx context.Context) error {
	// TODO: implement me!
	return ErrNotImplemented
}
