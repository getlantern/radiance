package account

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common/settings"
)

func TestFetchUserDataPreservesLogin(t *testing.T) {
	ac, server := newTestClientWithSRP(t, "test@example.com", "password")
	require.NoError(t, settings.Set(settings.DeviceIDKey, "deviceId"))
	server.loginResponse = &UserData{
		LegacyID:       123,
		LegacyToken:    "old-token",
		Id:             "account-id",
		EmailConfirmed: true,
		Success:        true,
		Token:          "jwt-token",
		Devices:        []*protos.LoginResponse_Device{{Id: "deviceId", Name: "Laptop"}},
		LegacyUserData: &protos.LoginResponse_UserData{
			UserId:     123,
			Token:      "old-token",
			UserLevel:  "pro",
			Expiration: 123456789,
			Devices:    []*protos.LoginResponse_Device{{Id: "old-device"}},
		},
	}
	ctx := context.Background()
	login, err := ac.Login(ctx, "test@example.com", "password")
	require.NoError(t, err)
	want := proto.Clone(login).(*UserData)
	want.LegacyToken = "test-token"
	want.LegacyUserData = &protos.LoginResponse_UserData{
		UserId:   123,
		Token:    "test-token",
		DeviceID: "deviceId",
	}

	for range 2 {
		got, err := ac.FetchUserData(ctx)
		require.NoError(t, err)
		assert.True(t, proto.Equal(want, got), "want %v, got %v", want, got)
		require.NoError(t, settings.Reload())
		var stored UserData
		require.NoError(t, settings.GetStruct(settings.UserDataKey, &stored))
		assert.True(t, proto.Equal(want, &stored), "want %v, stored %v", want, &stored)
	}

	user, err := ac.Logout(ctx, "test@example.com")
	require.NoError(t, err)
	want = &UserData{
		LegacyID:       123,
		LegacyToken:    "test-token",
		LegacyUserData: want.LegacyUserData,
	}
	assert.True(t, proto.Equal(want, user), "want %v, got %v", want, user)
	var stored UserData
	require.NoError(t, settings.GetStruct(settings.UserDataKey, &stored))
	assert.True(t, proto.Equal(want, &stored), "want %v, stored %v", want, &stored)
	assert.Empty(t, settings.GetString(settings.JwtTokenKey))
}

func TestFetchUserDataWithoutMatchingLogin(t *testing.T) {
	for _, tt := range []struct {
		name     string
		cachedID int64
		cache    bool
	}{
		{name: "no cached login"},
		{name: "different account", cachedID: 456, cache: true},
		{name: "unknown account", cache: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ac, _ := newTestClient(t)
			if tt.cache {
				ac.setData(&UserData{
					LegacyID:       tt.cachedID,
					Id:             "previous-account",
					EmailConfirmed: true,
					Success:        true,
					Token:          "previous-jwt",
					Devices:        []*protos.LoginResponse_Device{{Id: "old-device"}},
					LegacyUserData: &protos.LoginResponse_UserData{UserId: tt.cachedID},
				})
			}
			got, err := ac.FetchUserData(context.Background())
			require.NoError(t, err)
			want := &UserData{
				LegacyID:       123,
				LegacyToken:    "test-token",
				LegacyUserData: &protos.LoginResponse_UserData{UserId: 123, Token: "test-token"},
			}
			assert.True(t, proto.Equal(want, got), "want %v, got %v", want, got)
			var stored UserData
			require.NoError(t, settings.GetStruct(settings.UserDataKey, &stored))
			assert.True(t, proto.Equal(want, &stored), "want %v, stored %v", want, &stored)
		})
	}
}

func TestLoginReplacesCachedFields(t *testing.T) {
	ac, server := newTestClientWithSRP(t, "test@example.com", "password")
	ac.setData(&UserData{
		LegacyID:       123,
		Id:             "previous-id",
		EmailConfirmed: true,
		Success:        true,
		Token:          "previous-jwt",
		Devices:        []*protos.LoginResponse_Device{{Id: "old-device"}},
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 123},
	})
	server.loginResponse = &UserData{
		LegacyID:       123,
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 123},
	}
	got, err := ac.Login(context.Background(), "test@example.com", "password")
	require.NoError(t, err)
	assert.True(t, proto.Equal(server.loginResponse, got))
	var stored UserData
	require.NoError(t, settings.GetStruct(settings.UserDataKey, &stored))
	assert.True(t, proto.Equal(server.loginResponse, &stored))
}

func TestFetchUserDataInvalidCache(t *testing.T) {
	ac, _ := newTestClient(t)
	require.NoError(t, settings.Set(settings.UserDataKey, "invalid"))
	user, err := ac.FetchUserData(context.Background())
	require.ErrorContains(t, err, "reading cached user data")
	assert.Nil(t, user)
	assert.Equal(t, "invalid", settings.GetString(settings.UserDataKey))
}

func TestSetDataNilClearsCachedUser(t *testing.T) {
	ac, _ := newTestClient(t)
	_, err := ac.NewUser(context.Background())
	require.NoError(t, err)
	ac.setData(nil)
	assert.False(t, settings.Exists(settings.UserDataKey))
	assert.Empty(t, settings.GetString(settings.TokenKey))
}
