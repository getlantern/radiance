package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
)

const placeholderEmail = "support+42@getlantern.org"

type bindingServer struct {
	*testServer
	completeStatus int
	rejectBind     bool
	boundEmail     string
	completed      *protos.CompleteBindLegacyEmailRequest
}

// legacyBindingClient returns a client signed in to legacy account 42 with a
// placeholder email. Bind-complete and /user-data are served here and the rest
// go to the shared mock, so login runs SRP against the verifier the bind sent.
func legacyBindingClient(t *testing.T) (*Client, *bindingServer) {
	t.Helper()
	a, state := newTestClient(t)
	server := &bindingServer{testServer: state, completeStatus: http.StatusOK, boundEmail: placeholderEmail}
	upstream, err := url.Parse(a.authURL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/bind_legacy_email/complete":
			assertBindingHeaders(t, r)
			var req protos.CompleteBindLegacyEmailRequest
			require.NoError(t, readProtoRequest(r, &req))
			server.completed = &req
			if !server.rejectBind {
				state.salt[req.Email] = req.NewSalt
				state.verifier = req.NewVerifier
				server.boundEmail = req.Email
			}
			if server.completeStatus != http.StatusOK {
				http.Error(w, "bad gateway", server.completeStatus)
				return
			}
			writeProtoResponse(w, &protos.EmptyResponse{})
		case "/user-data":
			assertBindingHeaders(t, r)
			writeJSONResponse(w, UserDataResponse{
				BaseResponse:           &protos.BaseResponse{},
				LoginResponse_UserData: &protos.LoginResponse_UserData{UserId: 42, Email: server.boundEmail},
			})
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	a.authURL, a.proURL = ts.URL, ts.URL

	a.setData(&UserData{
		LegacyID: 42, LegacyToken: "pro-token",
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 42, Token: "pro-token", Email: placeholderEmail, UserLevel: "pro"},
	})
	require.NoError(t, settings.Set(settings.DeviceIDKey, "device"))
	state.loginResponse = &UserData{
		LegacyID: 42, LegacyToken: "pro-token", Success: true,
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 42, Token: "pro-token", Email: "real@example.com", UserLevel: "pro"},
	}
	return a, server
}

func assertBindingHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	assert.Equal(t, "42", r.Header.Get(common.UserIDHeader))
	assert.Equal(t, "pro-token", r.Header.Get(common.ProTokenHeader))
	assert.Equal(t, "device", r.Header.Get(common.DeviceIDHeader))
}

func bindingSettings(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(settings.GetAll())
	require.NoError(t, err)
	return string(data)
}

func TestStartBindLegacyEmailPreservesAccount(t *testing.T) {
	a, _ := newTestClient(t)
	a.setData(&UserData{LegacyID: 42, LegacyToken: "pro-token"})
	require.NoError(t, settings.Set(settings.DeviceIDKey, "device"))
	before := bindingSettings(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/users/bind_legacy_email", r.URL.Path)
		assertBindingHeaders(t, r)
		var req protos.StartBindLegacyEmailRequest
		assert.NoError(t, readProtoRequest(r, &req))
		assert.Equal(t, "real@example.com", req.Email)
		writeProtoResponse(w, &protos.EmptyResponse{})
	}))
	t.Cleanup(server.Close)
	a.authURL = server.URL

	require.NoError(t, a.StartBindLegacyEmail(t.Context(), " Real@Example.COM "))
	assert.JSONEq(t, before, bindingSettings(t))
}

func TestCompleteBindLegacyEmailSignsIn(t *testing.T) {
	a, server := legacyBindingClient(t)

	require.NoError(t, a.CompleteBindLegacyEmail(t.Context(), " Real@Example.COM ", "new-password", "123456"))
	require.NotNil(t, server.completed)
	assert.Equal(t, "real@example.com", server.completed.Email)
	assert.Equal(t, "123456", server.completed.Code)
	assert.Equal(t, int64(42), settings.GetInt64(settings.UserIDKey))
	assert.Equal(t, "real@example.com", settings.GetString(settings.EmailKey))
	assert.Equal(t, server.completed.NewSalt, a.getSaltCached())
}

func TestCompleteBindLegacyEmailFailedResponse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		committed bool
	}{
		{name: "binding committed", committed: true},
		{name: "binding not committed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, server := legacyBindingClient(t)
			server.completeStatus = http.StatusBadGateway
			server.rejectBind = !tc.committed
			before := bindingSettings(t)
			loginCalled := false
			server.beforeLogin = func() { loginCalled = true }

			err := a.CompleteBindLegacyEmail(t.Context(), "real@example.com", "new-password", "123456")
			if tc.committed {
				require.NoError(t, err)
				assert.True(t, loginCalled)
				assert.Equal(t, "real@example.com", settings.GetString(settings.EmailKey))
			} else {
				require.ErrorContains(t, err, "502")
				assert.False(t, loginCalled)
				assert.JSONEq(t, before, bindingSettings(t))
			}
		})
	}
}

func TestCompleteBindLegacyEmailLoginFailure(t *testing.T) {
	a, server := legacyBindingClient(t)
	server.loginResponse.LegacyID = 99

	err := a.CompleteBindLegacyEmail(t.Context(), "real@example.com", "new-password", "123456")
	require.ErrorIs(t, err, ErrBoundLoginRequired)
	assert.ErrorContains(t, err, "authenticated account does not match")
}

func TestBindLegacyEmailRequiresDeviceCredentials(t *testing.T) {
	for name, missing := range map[string]func() error{
		"user":   func() error { return settings.Clear(settings.UserIDKey) },
		"token":  func() error { return settings.Clear(settings.TokenKey) },
		"device": func() error { return settings.Clear(settings.DeviceIDKey) },
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := legacyBindingClient(t)
			require.NoError(t, missing())
			require.ErrorIs(t, a.StartBindLegacyEmail(t.Context(), "real@example.com"), ErrNotLoggedIn)
			require.ErrorIs(t, a.CompleteBindLegacyEmail(t.Context(), "real@example.com", "password", "123456"), ErrNotLoggedIn)
		})
	}
}

func TestBindLegacyEmailErrorKeepsServerMessageOnly(t *testing.T) {
	protoBody := protowire.AppendTag(nil, 1, protowire.VarintType)
	protoBody = protowire.AppendVarint(protoBody, 400)
	protoBody = protowire.AppendTag(protoBody, 2, protowire.BytesType)
	protoBody = protowire.AppendString(protoBody, "invalid or expired verification code")
	for name, tc := range map[string]struct {
		body []byte
		want string
	}{
		"json":     {[]byte(`{"msg":"invalid email","token":"pro-token"}`), "legacy email binding: HTTP 400: received bad response: invalid email"},
		"protobuf": {protoBody, "legacy email binding: HTTP 400: received bad response: invalid or expired verification code"},
		"other":    {[]byte(`<html>pro-token</html>`), "legacy email binding: HTTP 400"},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := newTestClient(t)
			a.setData(&UserData{LegacyID: 42, LegacyToken: "pro-token"})
			require.NoError(t, settings.Set(settings.DeviceIDKey, "device"))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(tc.body)
			}))
			t.Cleanup(server.Close)
			a.authURL = server.URL

			err := a.StartBindLegacyEmail(t.Context(), "real@example.com")
			require.EqualError(t, err, tc.want)
		})
	}
}
