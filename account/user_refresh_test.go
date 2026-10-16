package account

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		UserId:    123,
		Token:     "test-token",
		DeviceID:  "deviceId",
		UserLevel: "free",
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
					LegacyUserData: &protos.LoginResponse_UserData{
						UserId: tt.cachedID, Email: "old@example.com", UserLevel: "pro",
					},
				}, nil)
			}
			got, err := ac.FetchUserData(context.Background())
			require.NoError(t, err)
			want := &UserData{
				LegacyID:       123,
				LegacyToken:    "test-token",
				LegacyUserData: &protos.LoginResponse_UserData{UserId: 123, Token: "test-token", UserLevel: "free"},
			}
			assert.True(t, proto.Equal(want, got), "want %v, got %v", want, got)
			var stored UserData
			require.NoError(t, settings.GetStruct(settings.UserDataKey, &stored))
			assert.True(t, proto.Equal(want, &stored), "want %v, stored %v", want, &stored)
			assert.Empty(t, settings.GetString(settings.JwtTokenKey))
			assert.Empty(t, settings.GetString(settings.EmailKey))
			assert.False(t, settings.IsPro())
			devices, err := settings.Devices()
			require.NoError(t, err)
			assert.Empty(t, devices)
		})
	}
}

func TestFetchUserDataWithoutToken(t *testing.T) {
	for _, id := range []int64{123, 456, 0} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			ac, _ := newTestClient(t)
			ac.setData(&UserData{
				LegacyID: 123, LegacyToken: "cached-token", Id: "account-id",
				LegacyUserData: &protos.LoginResponse_UserData{UserId: 123, Token: "cached-token"},
			}, nil)
			require.NoError(t, settings.Set(settings.TokenKey, "current-token"))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSONResponse(w, UserDataResponse{
					LoginResponse_UserData: &protos.LoginResponse_UserData{UserId: id, UserLevel: "free"},
				})
			}))
			defer server.Close()
			ac.proURL = server.URL

			got, err := ac.FetchUserData(context.Background())
			require.NoError(t, err)
			wantToken := ""
			if id == 123 {
				wantToken = "current-token"
				assert.Equal(t, "account-id", got.Id)
			}
			assert.Equal(t, wantToken, got.LegacyToken)
			assert.Equal(t, wantToken, got.LegacyUserData.Token)
			require.NoError(t, settings.Reload())
			var stored UserData
			require.NoError(t, settings.GetStruct(settings.UserDataKey, &stored))
			assert.True(t, proto.Equal(got, &stored))
			if id != 0 {
				assert.Equal(t, wantToken, settings.GetString(settings.TokenKey))
			}
		})
	}
}

func TestSignupSwitchesAccountBeforeRefresh(t *testing.T) {
	ac, _ := newTestClient(t)
	ac.setData(&UserData{
		LegacyID: 456, LegacyToken: "old-token", Token: "old-jwt",
		Devices:        []*protos.LoginResponse_Device{{Id: "old-device"}},
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 456},
	}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProtoResponse(w, &protos.SignupResponse{LegacyID: 123, ProToken: "signup-token", Token: "signup-jwt"})
	}))
	defer server.Close()
	ac.authURL = server.URL
	_, _, err := ac.SignUp(context.Background(), "new@example.com", "password")
	require.NoError(t, err)
	_, err = ac.FetchUserData(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "signup-jwt", settings.GetString(settings.JwtTokenKey))
	devices, err := settings.Devices()
	require.NoError(t, err)
	assert.Empty(t, devices)
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
	}, nil)
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

func TestFetchUserDataReplacesInvalidCache(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cached any
	}{
		{name: "invalid object", cached: "invalid"},
		{name: "partially decoded object", cached: map[string]any{
			"legacyID":       123,
			"id":             "old-id",
			"emailConfirmed": true,
			"Success":        true,
			"legacyUserData": "invalid",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ac, _ := newTestClient(t)
			require.NoError(t, settings.Set(settings.UserIDKey, int64(123)))
			require.NoError(t, settings.Set(settings.UserDataKey, tt.cached))
			got, err := ac.FetchUserData(context.Background())
			require.NoError(t, err)
			want := &UserData{
				LegacyID:       123,
				LegacyToken:    "test-token",
				LegacyUserData: &protos.LoginResponse_UserData{UserId: 123, Token: "test-token", UserLevel: "free"},
			}
			assert.True(t, proto.Equal(want, got), "want %v, got %v", want, got)
			require.NoError(t, settings.Reload())
			var stored UserData
			require.NoError(t, settings.GetStruct(settings.UserDataKey, &stored))
			assert.True(t, proto.Equal(want, &stored), "want %v, stored %v", want, &stored)
		})
	}
}

func TestSetDataNilClearsCachedUser(t *testing.T) {
	ac, _ := newTestClient(t)
	_, err := ac.NewUser(context.Background())
	require.NoError(t, err)
	ac.setData(nil, nil)
	assert.False(t, settings.Exists(settings.UserDataKey))
	assert.Empty(t, settings.GetString(settings.TokenKey))
}

func TestFetchUserDataDoesNotReplayLoginSettings(t *testing.T) {
	ac, _ := newTestClient(t)
	ac.setData(&UserData{
		LegacyID:       123,
		Token:          "old-jwt",
		Devices:        []*protos.LoginResponse_Device{{Id: "old-device"}},
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 123},
	}, nil)
	devices := []settings.Device{{ID: "new-device", Name: "Laptop"}}
	require.NoError(t, settings.Patch(settings.Settings{
		settings.JwtTokenKey: "new-jwt",
		settings.DevicesKey:  devices,
	}))
	_, err := ac.FetchUserData(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "new-jwt", settings.GetString(settings.JwtTokenKey))
	storedDevices, err := settings.Devices()
	require.NoError(t, err)
	assert.Equal(t, devices, storedDevices)
	assert.Equal(t, "test-token", settings.GetString(settings.TokenKey))

	require.NoError(t, settings.Set(settings.UserDataKey, &UserData{LegacyID: 456, Token: "old-jwt"}))
	_, err = ac.FetchUserData(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "new-jwt", settings.GetString(settings.JwtTokenKey))
	storedDevices, err = settings.Devices()
	require.NoError(t, err)
	assert.Equal(t, devices, storedDevices)
}

func TestStaleUserDataIsDiscarded(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(a *Client)
	}{
		{"A to B", func(a *Client) {
			a.setData(&UserData{LegacyID: 2, LegacyToken: "b"}, nil)
		}},
		{"A to B to A", func(a *Client) {
			a.setData(&UserData{LegacyID: 2, LegacyToken: "b"}, nil)
			a.setData(&UserData{LegacyID: 1, LegacyToken: "a"}, nil)
		}},
		{"logout", func(a *Client) { a.ClearUser() }},
		{"settings patch", func(a *Client) {
			a.PatchAccountSettings(settings.Settings{settings.UserIDKey: int64(2), settings.TokenKey: "b"})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := newTestClient(t)
			a.setData(&UserData{LegacyID: 1, LegacyToken: "a"}, nil)
			handler, requested, release := heldJSON(UserDataResponse{
				LoginResponse_UserData: &protos.LoginResponse_UserData{UserId: 1, Token: "a", UserLevel: "pro"},
			})
			useProServer(t, a, handler)

			errc := make(chan error, 1)
			go func() {
				_, err := a.FetchUserData(context.Background())
				errc <- err
			}()
			<-requested
			tt.change(a)
			after := currentAccount()
			close(release)

			assert.ErrorIs(t, <-errc, ErrAccountChanged)
			assert.Equal(t, after, currentAccount(), "a stale response must not change the account")
			assert.False(t, settings.IsPro())
		})
	}
}

func TestNewUserDiscardsInFlightUserData(t *testing.T) {
	a, _ := newTestClient(t)
	a.setData(&UserData{LegacyID: 1, LegacyToken: "a"}, nil)
	userData, requested, release := heldJSON(UserDataResponse{
		LoginResponse_UserData: &protos.LoginResponse_UserData{UserId: 1, Token: "a"},
	})
	mux := http.NewServeMux()
	mux.Handle("/user-data", userData)
	mux.Handle("/user-create", serveJSON(UserDataResponse{
		LoginResponse_UserData: &protos.LoginResponse_UserData{UserId: 2, Token: "b"},
	}))
	useProServer(t, a, mux)

	errc := make(chan error, 1)
	go func() {
		_, err := a.FetchUserData(context.Background())
		errc <- err
	}()
	<-requested
	_, err := a.NewUser(context.Background())
	require.NoError(t, err)
	close(release)

	assert.ErrorIs(t, <-errc, ErrAccountChanged)
	assert.Equal(t, int64(2), settings.GetInt64(settings.UserIDKey))
}

func TestReturnedUserDataIsACopy(t *testing.T) {
	a, _ := newTestClient(t)
	got, err := a.FetchUserData(context.Background())
	require.NoError(t, err)

	got.LegacyUserData.UserLevel = "pro"
	got.Id = "mutated"

	var stored UserData
	require.NoError(t, settings.GetStruct(settings.UserDataKey, &stored))
	assert.Empty(t, stored.Id)
	assert.Equal(t, "free", stored.LegacyUserData.UserLevel)
}

func TestAccountSwitchNeverExposesAnEmptyIdentity(t *testing.T) {
	a, _ := newTestClient(t)
	a.setData(&UserData{LegacyID: 1, LegacyToken: "a"}, nil)
	stop := make(chan struct{})
	sawEmpty := make(chan bool, 1)
	go func() {
		empty := false
		for {
			select {
			case <-stop:
				sawEmpty <- empty
				return
			default:
				empty = empty || settings.GetInt64(settings.UserIDKey) == 0 || settings.GetString(settings.TokenKey) == ""
			}
		}
	}()
	for i := range 50 {
		a.setData(&UserData{LegacyID: int64(2 + i%2), LegacyToken: "t"}, nil)
	}
	close(stop)
	assert.False(t, <-sawEmpty, "readers must never see an account without an id or token")
}

func TestPatchAccountSwitchClearsOld(t *testing.T) {
	for _, id := range []any{
		int(2), int8(2), int16(2), int32(2), int64(2),
		uint(2), uint8(2), uint16(2), uint32(2), uint64(2),
		float64(2), "2",
	} {
		t.Run(fmt.Sprintf("%T", id), func(t *testing.T) {
			a, _ := newTestClient(t)
			require.NoError(t, a.setData(&UserData{
				LegacyID:       1,
				LegacyToken:    "a",
				Token:          "a-jwt",
				LegacyUserData: &protos.LoginResponse_UserData{UserId: 1, Email: "a@example.com", UserLevel: "pro"},
			}, nil))
			require.NoError(t, a.storeSalt([]byte("a-salt")))
			setOAuthFlags(t)
			updates := settings.Settings{settings.UserIDKey: id, settings.TokenKey: "b"}

			require.NoError(t, a.PatchAccountSettings(updates))

			assert.Equal(t, id, updates[settings.UserIDKey])
			assert.Equal(t, int64(2), settings.Get(settings.UserIDKey))
			require.NoError(t, settings.Reload())
			assert.Equal(t, int64(2), settings.GetInt64(settings.UserIDKey))
			assert.Equal(t, "b", settings.GetString(settings.TokenKey))
			assert.Empty(t, settings.GetString(settings.JwtTokenKey))
			assert.Empty(t, settings.GetString(settings.EmailKey))
			assert.Empty(t, settings.Get(settings.UserDataKey))
			assert.False(t, settings.IsPro())
			assert.False(t, settings.GetBool(settings.OAuthLoginKey))
			assert.Empty(t, settings.GetString(settings.OAuthProviderKey))
			assert.Nil(t, a.getSaltCached())
			salt, err := readSalt(a.saltPath)
			require.NoError(t, err)
			assert.Nil(t, salt)
		})
	}
}

func TestPatchAccountRejectsBadID(t *testing.T) {
	for _, id := range []any{
		nil, true, "", "invalid", "2.5", "9223372036854775808",
		0, -1, uint64(math.MaxInt64) + 1, 2.5, float32(2.5),
		float64(math.MaxInt64), math.Inf(1), math.NaN(),
	} {
		t.Run(fmt.Sprintf("%T/%v", id, id), func(t *testing.T) {
			a, _ := newTestClient(t)
			require.NoError(t, a.PatchAccountSettings(settings.Settings{
				settings.UserIDKey: int64(1), settings.TokenKey: "a",
			}))
			before := settings.GetAll()
			generation := a.identityGeneration()

			err := a.PatchAccountSettings(settings.Settings{settings.UserIDKey: id, settings.TokenKey: "b"})

			require.ErrorContains(t, err, "invalid user ID")
			assert.Equal(t, before, settings.GetAll())
			assert.Equal(t, generation, a.identityGeneration())
		})
	}
}

func TestParseUserIDLarge(t *testing.T) {
	for _, tt := range []struct {
		value any
		want  int64
	}{
		{float64(1e9), 1e9},
		{float64(1<<60) + 256, 1152921504606847232},
		{float64(math.MaxInt64) - 1024, 9223372036854774784},
		{int64(math.MaxInt64), math.MaxInt64},
		{uint64(math.MaxInt64), math.MaxInt64},
		{"9223372036854775807", math.MaxInt64},
	} {
		got, err := parseUserID(tt.value)
		require.NoError(t, err)
		assert.Equal(t, tt.want, got)
	}
}

func TestSwitchNeedsSaltCleared(t *testing.T) {
	a, _ := newTestClient(t)
	require.NoError(t, a.PatchAccountSettings(settings.Settings{
		settings.UserIDKey: int64(1), settings.TokenKey: "a",
	}))
	require.NoError(t, a.storeSalt([]byte("a-salt")))
	before := settings.GetAll()
	dir := filepath.Dir(a.saltPath)
	require.NoError(t, os.Rename(dir, dir+".aside"))
	t.Cleanup(func() { require.NoError(t, os.Rename(dir+".aside", dir)) })

	err := a.PatchAccountSettings(settings.Settings{settings.UserIDKey: int64(2), settings.TokenKey: "b"})

	require.ErrorContains(t, err, "clearing the previous account's salt")
	assert.Equal(t, before, settings.GetAll())
	assert.Equal(t, []byte("a-salt"), a.getSaltCached())
	salt, err := readSalt(filepath.Join(dir+".aside", saltFileName))
	require.NoError(t, err)
	assert.Equal(t, []byte("a-salt"), salt)
}

func TestPatchAccountNoopKeepsPending(t *testing.T) {
	a, _ := newTestClient(t)
	require.NoError(t, a.PatchAccountSettings(settings.Settings{
		settings.UserIDKey: int64(1), settings.TokenKey: "a", settings.OAuthLoginKey: true,
	}))
	require.NoError(t, settings.Reload())
	handler, requested, release := heldJSON(UserDataResponse{
		LoginResponse_UserData: &protos.LoginResponse_UserData{UserId: 1, Token: "a", UserLevel: "pro"},
	})
	useProServer(t, a, handler)
	errc := make(chan error, 1)
	go func() {
		_, err := a.FetchUserData(context.Background())
		errc <- err
	}()
	<-requested

	require.NoError(t, a.PatchAccountSettings(settings.Settings{
		settings.UserIDKey: "1", settings.TokenKey: "a", settings.OAuthLoginKey: true,
	}))
	close(release)

	require.NoError(t, <-errc)
	assert.True(t, settings.IsPro())
}

func TestPatchAccountSameKeepsFields(t *testing.T) {
	a, _ := newTestClient(t)
	require.NoError(t, a.setData(&UserData{
		LegacyID:       1,
		LegacyToken:    "a",
		Token:          "a-jwt",
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 1, Email: "a@example.com"},
	}, nil))
	require.NoError(t, a.storeSalt([]byte("a-salt")))

	require.NoError(t, a.PatchAccountSettings(settings.Settings{settings.EmailKey: "new@example.com"}))

	assert.Equal(t, "new@example.com", settings.GetString(settings.EmailKey))
	assert.Equal(t, "a-jwt", settings.GetString(settings.JwtTokenKey))
	assert.Equal(t, []byte("a-salt"), a.getSaltCached())
}

func TestFetchUserDataKeepsFreeLevel(t *testing.T) {
	ac, _ := newTestClient(t)
	ac.setData(&UserData{
		LegacyID: 123, LegacyToken: "test-token",
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 123, UserLevel: "free"},
	}, nil)
	data, err := ac.FetchUserData(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "free", data.LegacyUserData.UserLevel)
	assert.Equal(t, "free", settings.GetString(settings.UserLevelKey))
	require.NoError(t, settings.Reload())
	var cached UserData
	require.NoError(t, settings.GetStruct(settings.UserDataKey, &cached))
	assert.Equal(t, "free", cached.LegacyUserData.UserLevel)
}
