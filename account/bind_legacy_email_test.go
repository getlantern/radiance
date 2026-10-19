package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
)

func legacyBindingClient(t *testing.T) (*Client, *UserData) {
	t.Helper()
	a, _ := newTestClient(t)
	data := &UserData{
		LegacyID: 42, LegacyToken: "pro-token", Id: "account-id", Token: "jwt-token",
		Devices: []*protos.LoginResponse_Device{{Id: "device", Name: "Laptop"}},
		LegacyUserData: &protos.LoginResponse_UserData{
			UserId: 42, Token: "pro-token", Email: "support+42@getlantern.org",
			UserLevel: "pro", Expiration: 2000000000, Subscription: "subscription",
			DeviceID: "device", Devices: []*protos.LoginResponse_Device{{Id: "device"}},
		},
	}
	a.setData(data)
	require.NoError(t, settings.Set(settings.DeviceIDKey, "device"))
	a.setSalt([]byte("original-salt"))
	require.NoError(t, writeSalt(a.getSaltCached(), a.saltPath))
	return a, data
}

func bindingSettings(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(settings.GetAll())
	require.NoError(t, err)
	return string(data)
}

func assertBindingHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	assert.Equal(t, "42", r.Header.Get(common.UserIDHeader))
	assert.Equal(t, "pro-token", r.Header.Get(common.ProTokenHeader))
	assert.Equal(t, "device", r.Header.Get(common.DeviceIDHeader))
	assert.Equal(t, "application/x-protobuf", r.Header.Get("Content-Type"))
}

func TestStartBindLegacyEmailPreservesAccount(t *testing.T) {
	a, _ := legacyBindingClient(t)
	before := bindingSettings(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/users/bind_legacy_email", r.URL.Path)
		assertBindingHeaders(t, r)
		var request protos.StartBindLegacyEmailRequest
		assert.NoError(t, readProtoRequest(r, &request))
		assert.Equal(t, "real@example.com", request.Email)
		writeProtoResponse(w, &protos.EmptyResponse{})
	}))
	t.Cleanup(server.Close)
	a.authURL = server.URL
	require.NoError(t, a.StartBindLegacyEmail(t.Context(), " Real@Example.COM "))
	assert.JSONEq(t, before, bindingSettings(t))
	assert.Equal(t, []byte("original-salt"), a.getSaltCached())
}

func TestCompleteBindLegacyEmailPreservesIdentityAndNotifies(t *testing.T) {
	a, original := legacyBindingClient(t)
	before := settings.GetAllFor(settings.UserIDKey, settings.TokenKey, settings.DeviceIDKey,
		settings.JwtTokenKey, settings.UserLevelKey, settings.DevicesKey)
	changes := make(chan struct{}, 2)
	subscription := events.Subscribe(func(UserChangeEvent) { changes <- struct{}{} })
	t.Cleanup(subscription.Unsubscribe)
	var request protos.CompleteBindLegacyEmailRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/users/bind_legacy_email/complete", r.URL.Path)
		assertBindingHeaders(t, r)
		assert.NoError(t, readProtoRequest(r, &request))
		assert.Equal(t, "real@example.com", request.Email)
		assert.Equal(t, "123456", request.Code)
		assert.Len(t, request.NewSalt, 16)
		writeProtoResponse(w, &protos.EmptyResponse{})
	}))
	t.Cleanup(server.Close)
	a.authURL = server.URL
	require.NoError(t, a.CompleteBindLegacyEmail(t.Context(), " Real@Example.COM ", "new-password", "123456"))
	srpClient, err := newSRPClient("real@example.com", "new-password", request.NewSalt)
	require.NoError(t, err)
	verifier, err := srpClient.Verifier()
	require.NoError(t, err)
	assert.Equal(t, verifier.Bytes(), request.NewVerifier)
	assert.Equal(t, before, settings.GetAllFor(settings.UserIDKey, settings.TokenKey, settings.DeviceIDKey,
		settings.JwtTokenKey, settings.UserLevelKey, settings.DevicesKey))
	assert.Equal(t, "real@example.com", settings.GetString(settings.EmailKey))
	var cached UserData
	require.NoError(t, settings.GetStruct(settings.UserDataKey, &cached))
	want := proto.Clone(original).(*UserData)
	want.EmailConfirmed = true
	want.LegacyUserData.Email = "real@example.com"
	want.LegacyUserData.UnpassRegistered = true
	assert.True(t, proto.Equal(want, &cached))
	assert.Equal(t, request.NewSalt, a.getSaltCached())
	diskSalt, err := os.ReadFile(a.saltPath)
	require.NoError(t, err)
	assert.Equal(t, request.NewSalt, diskSalt)
	require.NoError(t, settings.Reload())
	require.NoError(t, settings.GetStruct(settings.UserDataKey, &cached))
	assert.True(t, proto.Equal(want, &cached))
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("account change event not received")
	}
	select {
	case <-changes:
		t.Fatal("unexpected extra account change event")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestBindLegacyEmailFailuresPreserveAccount(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "complete"}[complete], func(t *testing.T) {
			a, _ := legacyBindingClient(t)
			before := bindingSettings(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertBindingHeaders(t, r)
				http.Error(w, "rejected", http.StatusBadRequest)
			}))
			t.Cleanup(server.Close)
			a.authURL = server.URL
			var err error
			if complete {
				err = a.CompleteBindLegacyEmail(t.Context(), "real@example.com", "password", "000000")
			} else {
				err = a.StartBindLegacyEmail(t.Context(), "real@example.com")
			}
			require.Error(t, err)
			assert.JSONEq(t, before, bindingSettings(t))
			assert.Equal(t, []byte("original-salt"), a.getSaltCached())
			diskSalt, err := os.ReadFile(a.saltPath)
			require.NoError(t, err)
			assert.Equal(t, []byte("original-salt"), diskSalt)
		})
	}
}

func TestCompleteBindLegacyEmailRejectsIdentitySwitch(t *testing.T) {
	for _, field := range []string{"user", "token", "device"} {
		t.Run(field, func(t *testing.T) {
			a, _ := legacyBindingClient(t)
			var switched string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertBindingHeaders(t, r)
				switch field {
				case "user":
					assert.NoError(t, a.storeIdentity(99, "other-token"))
				case "token":
					assert.NoError(t, a.storeIdentity(42, "other-token"))
				case "device":
					assert.NoError(t, settings.Set(settings.DeviceIDKey, "other-device"))
				}
				switched = bindingSettings(t)
				writeProtoResponse(w, &protos.EmptyResponse{})
			}))
			t.Cleanup(server.Close)
			a.authURL = server.URL
			require.ErrorContains(t, a.CompleteBindLegacyEmail(t.Context(), "real@example.com", "password", "123456"), "account changed")
			assert.JSONEq(t, switched, bindingSettings(t))
			assert.Equal(t, []byte("original-salt"), a.getSaltCached())
			diskSalt, err := os.ReadFile(a.saltPath)
			require.NoError(t, err)
			assert.Equal(t, []byte("original-salt"), diskSalt)
		})
	}
}

func TestBindLegacyEmailRequiresDeviceCredentials(t *testing.T) {
	for _, missing := range []string{"user", "token", "device"} {
		t.Run(missing, func(t *testing.T) {
			a, _ := legacyBindingClient(t)
			switch missing {
			case "user":
				require.NoError(t, settings.Clear(settings.UserIDKey))
			case "token":
				require.NoError(t, settings.Clear(settings.TokenKey))
			case "device":
				require.NoError(t, settings.Clear(settings.DeviceIDKey))
			}
			require.ErrorIs(t, a.StartBindLegacyEmail(t.Context(), "real@example.com"), ErrNotLoggedIn)
			require.ErrorIs(t, a.CompleteBindLegacyEmail(t.Context(), "real@example.com", "password", "123456"), ErrNotLoggedIn)
		})
	}
}
