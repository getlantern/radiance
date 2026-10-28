package account

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common/settings"
)

func TestLoginAccountGuard(t *testing.T) {
	for _, tc := range []struct {
		name       string
		expectedID int64
		responseID int64
		guarded    bool
		success    bool
		noUserData bool
		wantError  string
	}{
		{name: "ordinary login can change accounts", responseID: 456, success: true},
		{name: "ordinary login preserves device limit handling", responseID: 456, noUserData: true},
		{name: "guarded login", expectedID: 123, responseID: 123, guarded: true, success: true},
		{name: "different current account", expectedID: 456, responseID: 456, guarded: true, wantError: "active account"},
		{name: "different authenticated account", expectedID: 123, responseID: 456, guarded: true, wantError: "authenticated account"},
		{name: "device limit", expectedID: 123, responseID: 123, guarded: true, noUserData: true, wantError: "did not complete"},
		{name: "unsuccessful response", expectedID: 123, responseID: 123, guarded: true, wantError: "did not complete"},
		{name: "missing account data", expectedID: 123, responseID: 123, guarded: true, success: true, noUserData: true, wantError: "did not complete"},
		{name: "missing expected account", guarded: true, wantError: "must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ac, server := newTestClientWithSRP(t, "test@example.com", "password")
			ac.setData(&UserData{
				LegacyID: 123, LegacyToken: "original-token",
				LegacyUserData: &protos.LoginResponse_UserData{UserId: 123, Email: "old@example.com", UserLevel: "pro", Expiration: 123456789},
			})
			server.loginResponse = &UserData{
				LegacyID: tc.responseID, LegacyToken: "login-token", Success: tc.success,
				LegacyUserData: &protos.LoginResponse_UserData{UserId: tc.responseID, Email: "test@example.com"},
			}
			if tc.noUserData {
				server.loginResponse.LegacyUserData = nil
			}
			oldSalt := []byte("previous-account-salt")
			ac.setSalt(oldSalt)
			require.NoError(t, writeSalt(oldSalt, ac.saltPath))
			before := settings.GetAll()

			var err error
			if tc.guarded {
				_, err = ac.LoginForAccount(t.Context(), "test@example.com", "password", tc.expectedID)
			} else {
				_, err = ac.Login(t.Context(), "test@example.com", "password")
			}
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Equal(t, before, settings.GetAll())
				require.Equal(t, oldSalt, ac.getSaltCached())
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.responseID, settings.GetInt64(settings.UserIDKey))
				if !tc.noUserData {
					require.Equal(t, "test@example.com", settings.GetString(settings.EmailKey))
				}
				require.Equal(t, server.salt["test@example.com"], ac.getSaltCached())
			}
			savedSalt, err := readSalt(ac.saltPath)
			require.NoError(t, err)
			require.Equal(t, ac.getSaltCached(), savedSalt)
		})
	}
}

func TestLoginRejectsChangedIdentity(t *testing.T) {
	for _, field := range []string{"account", "token", "device", "logout", "ordinary login"} {
		t.Run(field, func(t *testing.T) {
			ac, server := newTestClientWithSRP(t, "test@example.com", "password")
			require.NoError(t, ac.storeIdentity(123, "original-token"))
			require.NoError(t, settings.Set(settings.DeviceIDKey, "original-device"))
			server.loginResponse = &UserData{
				LegacyID: 123, LegacyToken: "login-token", Success: true,
				LegacyUserData: &protos.LoginResponse_UserData{UserId: 123},
			}
			oldSalt := []byte("original-salt")
			ac.setSalt(oldSalt)
			require.NoError(t, writeSalt(oldSalt, ac.saltPath))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			server.beforeLogin = func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			result := make(chan error, 1)
			go func() {
				var err error
				if field == "ordinary login" {
					_, err = ac.Login(ctx, "test@example.com", "password")
				} else {
					_, err = ac.LoginForAccount(ctx, "test@example.com", "password", 123)
				}
				result <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("login did not reach the server")
			}
			switch field {
			case "account", "ordinary login":
				ac.setData(&UserData{LegacyID: 456, LegacyToken: "replacement-token"})
			case "token":
				require.NoError(t, ac.storeIdentity(123, "replacement-token"))
			case "device":
				require.NoError(t, settings.Set(settings.DeviceIDKey, "replacement-device"))
			case "logout":
				ac.ClearUser()
			}
			before := settings.GetAll()
			close(release)
			require.ErrorContains(t, <-result, "account changed during login")
			require.Equal(t, before, settings.GetAll())
			require.Equal(t, oldSalt, ac.getSaltCached())
			savedSalt, err := readSalt(ac.saltPath)
			require.NoError(t, err)
			require.Equal(t, oldSalt, savedSalt)
		})
	}
}
