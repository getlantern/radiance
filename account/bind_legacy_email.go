package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
)

type bindingIdentity struct {
	userID   int64
	token    string
	deviceID string
}

func currentBindingIdentity() bindingIdentity {
	return bindingIdentity{
		userID:   settings.GetInt64(settings.UserIDKey),
		token:    settings.GetString(settings.TokenKey),
		deviceID: settings.GetString(settings.DeviceIDKey),
	}
}

func (a *Client) bindingIdentity() (bindingIdentity, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	identity := currentBindingIdentity()
	if identity.userID <= 0 || identity.token == "" || identity.deviceID == "" {
		return bindingIdentity{}, ErrNotLoggedIn
	}
	return identity, nil
}

func (i bindingIdentity) headers() map[string]string {
	return map[string]string{
		common.UserIDHeader:   strconv.FormatInt(i.userID, 10),
		common.ProTokenHeader: i.token,
		common.DeviceIDHeader: i.deviceID,
	}
}

// StartBindLegacyEmail sends a verification code without changing the current account.
func (a *Client) StartBindLegacyEmail(ctx context.Context, email string) error {
	identity, err := a.bindingIdentity()
	if err != nil {
		return err
	}
	data := &protos.StartBindLegacyEmailRequest{Email: strings.ToLower(strings.TrimSpace(email))}
	_, err = a.sendRequest(ctx, http.MethodPost, "/users/bind_legacy_email", nil, identity.headers(), data)
	return err
}

// CompleteBindLegacyEmail binds a verified email without replacing the active account or its entitlements.
func (a *Client) CompleteBindLegacyEmail(ctx context.Context, email, password, code string) error {
	identity, err := a.bindingIdentity()
	if err != nil {
		return err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	salt, err := generateSalt()
	if err != nil {
		return err
	}
	srpClient, err := newSRPClient(email, password, salt)
	if err != nil {
		return err
	}
	verifier, err := srpClient.Verifier()
	if err != nil {
		return err
	}
	data := &protos.CompleteBindLegacyEmailRequest{
		Email:       email,
		Code:        code,
		NewSalt:     salt,
		NewVerifier: verifier.Bytes(),
	}
	if _, err := a.sendRequest(ctx, http.MethodPost, "/users/bind_legacy_email/complete", nil, identity.headers(), data); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if currentBindingIdentity() != identity {
		return errors.New("account changed while binding email")
	}
	updates := settings.Settings{settings.EmailKey: email}
	var cached UserData
	if err := settings.GetStruct(settings.UserDataKey, &cached); err == nil && cached.LegacyID == identity.userID {
		cached.EmailConfirmed = true
		if cached.LegacyUserData != nil {
			cached.LegacyUserData.Email = email
			cached.LegacyUserData.UnpassRegistered = true
		}
		updates[settings.UserDataKey] = &cached
	}
	if err := writeSalt(salt, a.saltPath); err != nil {
		return err
	}
	a.salt = salt
	if err := settings.Patch(updates); err != nil {
		return fmt.Errorf("saving bound email: %w", err)
	}
	events.Emit(UserChangeEvent{})
	return nil
}
