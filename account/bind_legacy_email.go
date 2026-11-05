package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/traces"
)

// ErrBoundLoginRequired means the email and password were bound to the
// account but signing in afterwards failed. Retrying the login can succeed;
// retrying the binding can't, since its code is spent. The app matches this
// text across IPC.
var ErrBoundLoginRequired = errors.New("legacy_email_bound_login_required")

func (a *Client) bindingIdentity() (loginIdentity, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	identity := currentLoginIdentity()
	if identity.userID <= 0 || identity.token == "" || identity.deviceID == "" {
		return loginIdentity{}, ErrNotLoggedIn
	}
	return identity, nil
}

func (i loginIdentity) headers() map[string]string {
	return map[string]string{
		common.UserIDHeader:   strconv.FormatInt(i.userID, 10),
		common.ProTokenHeader: i.token,
		common.DeviceIDHeader: i.deviceID,
	}
}

// StartBindLegacyEmail sends a verification code to email without changing the current account.
func (a *Client) StartBindLegacyEmail(ctx context.Context, email string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "start_bind_legacy_email")
	defer span.End()

	identity, err := a.bindingIdentity()
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	data := &protos.StartBindLegacyEmailRequest{Email: strings.ToLower(strings.TrimSpace(email))}
	_, err = a.sendRequest(ctx, http.MethodPost, "/users/bind_legacy_email", nil, identity.headers(), data)
	return traces.RecordError(ctx, bindError(err))
}

// CompleteBindLegacyEmail binds email and password to the current legacy
// account, then signs in to it. It returns [ErrBoundLoginRequired] if sign-in
// fails after a confirmed bind; other errors may still leave the email bound.
func (a *Client) CompleteBindLegacyEmail(ctx context.Context, email, password, code string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "complete_bind_legacy_email")
	defer span.End()

	identity, err := a.bindingIdentity()
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	email = strings.ToLower(strings.TrimSpace(email))
	salt, err := generateSalt()
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	srpClient, err := newSRPClient(email, password, salt)
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	verifier, err := srpClient.Verifier()
	if err != nil {
		return traces.RecordError(ctx, err)
	}
	data := &protos.CompleteBindLegacyEmailRequest{
		Email:       email,
		Code:        code,
		NewSalt:     salt,
		NewVerifier: verifier.Bytes(),
	}
	_, err = a.sendRequest(ctx, http.MethodPost, "/users/bind_legacy_email/complete", nil, identity.headers(), data)
	// The server may commit the binding and still fail the response, after
	// which the code is spent, so check whether the email landed.
	if err != nil && (ctx.Err() != nil || !a.emailBound(ctx, identity, email)) {
		return traces.RecordError(ctx, bindError(err))
	}
	if _, err := a.login(ctx, email, password, identity.userID); err != nil {
		return traces.RecordError(ctx, fmt.Errorf("%w: %w", ErrBoundLoginRequired, err))
	}
	return nil
}

func (a *Client) emailBound(ctx context.Context, identity loginIdentity, email string) bool {
	resp, err := a.sendRequest(ctx, http.MethodGet, a.proBaseURL()+"/user-data", nil, identity.headers(), nil)
	if err != nil {
		return false
	}
	var data UserDataResponse
	if json.Unmarshal(resp, &data) != nil || data.LoginResponse_UserData == nil {
		return false
	}
	return data.UserId == identity.userID && strings.EqualFold(data.Email, email)
}

// bindError keeps only the server's message from a failed response, since the
// app shows it to the user and the full body may echo credentials.
func bindError(err error) error {
	var resp *responseError
	if !errors.As(err, &resp) {
		return err
	}
	if msg := serverMessage(resp.body); msg != "" {
		return fmt.Errorf("legacy email binding: HTTP %d: received bad response: %s", resp.status, msg)
	}
	return fmt.Errorf("legacy email binding: HTTP %d", resp.status)
}

// serverMessage reads msg from the auth server's error body, which is JSON or
// protobuf (msg is field 2) depending on what the request accepted.
func serverMessage(body []byte) string {
	var data struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal(body, &data) == nil {
		return string(sanitizeResponseBody([]byte(data.Msg)))
	}
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			return ""
		}
		body = body[n:]
		if num == 2 && typ == protowire.BytesType {
			msg, n := protowire.ConsumeBytes(body)
			if n < 0 {
				return ""
			}
			return string(sanitizeResponseBody(msg))
		}
		n = protowire.ConsumeFieldValue(num, typ, body)
		if n < 0 {
			return ""
		}
		body = body[n:]
	}
	return ""
}
