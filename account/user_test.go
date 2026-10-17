package account

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/1Password/srp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/log"
)

// testServer holds server-side SRP state for the mock auth server.
type testServer struct {
	loginResponse                                *protos.LoginResponse
	salt                                         map[string][]byte
	verifier                                     []byte
	cache                                        map[string]string
	paymentRedirectIdempotencyKey                string
	paymentRedirectHasIdempotencyKey             bool
	paymentRedirectCouponCode                    string
	paymentRedirectHasCouponCode                 bool
	paymentRedirectAppVersion                    string
	paymentRedirectLibraryVersion                string
	subscriptionPaymentRedirectIdempotencyKey    string
	subscriptionPaymentRedirectHasIdempotencyKey bool
	subscriptionPaymentRedirectCouponCode        string
	subscriptionPaymentRedirectHasCouponCode     bool
	stripeSubscriptionCouponCode                 string
	stripeSubscriptionHasCouponCode              bool
	referralAttachV1Code                         string
	referralAttachV1Error                        string
	referralAttachV2Code                         string
	referralAttachV2Channel                      string
	referralAttachV2Error                        string
	paymentRedirectResponse                      any
}

func writeProtoResponse(w http.ResponseWriter, msg proto.Message) {
	data, err := proto.Marshal(msg)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Write(data)
}

func readProtoRequest(r *http.Request, msg proto.Message) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return proto.Unmarshal(data, msg)
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func newTestServer(t *testing.T) (*httptest.Server, *testServer) {
	state := &testServer{
		salt:  make(map[string][]byte),
		cache: make(map[string]string),
		loginResponse: &protos.LoginResponse{
			LegacyUserData: &protos.LoginResponse_UserData{DeviceID: "deviceId"},
		},
	}
	mux := http.NewServeMux()

	// Auth endpoints
	mux.HandleFunc("/users/salt", func(w http.ResponseWriter, r *http.Request) {
		email := r.URL.Query().Get("email")
		salt := state.salt[email]
		if salt == nil {
			salt = []byte("salt")
		}
		writeProtoResponse(w, &protos.GetSaltResponse{Salt: salt})
	})

	mux.HandleFunc("/users/signup", func(w http.ResponseWriter, r *http.Request) {
		var req protos.SignupRequest
		if err := readProtoRequest(r, &req); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		state.salt[req.Email] = req.Salt
		state.verifier = req.Verifier
		writeProtoResponse(w, &protos.SignupResponse{})
	})

	mux.HandleFunc("/users/prepare", func(w http.ResponseWriter, r *http.Request) {
		var req protos.PrepareRequest
		if err := readProtoRequest(r, &req); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		A := big.NewInt(0).SetBytes(req.A)
		verifier := big.NewInt(0).SetBytes(state.verifier)
		server := srp.NewSRPServer(srp.KnownGroups[srp.RFC5054Group3072], verifier, nil)
		if err := server.SetOthersPublic(A); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		B := server.EphemeralPublic()
		if B == nil {
			http.Error(w, "cannot generate B", 500)
			return
		}
		if _, err := server.Key(); err != nil {
			http.Error(w, "cannot generate key", 500)
			return
		}
		proof, err := server.M(state.salt[req.Email], req.Email)
		if err != nil {
			http.Error(w, "cannot generate proof", 500)
			return
		}
		serverState, _ := server.MarshalBinary()
		state.cache[req.Email] = hex.EncodeToString(serverState)
		writeProtoResponse(w, &protos.PrepareResponse{B: B.Bytes(), Proof: proof})
	})

	mux.HandleFunc("/users/login", func(w http.ResponseWriter, r *http.Request) {
		writeProtoResponse(w, state.loginResponse)
	})

	// Simple auth endpoints that return empty responses
	for _, path := range []string{
		"/users/signup/resend/email",
		"/users/signup/complete/email",
		"/users/recovery/start/email",
		"/users/recovery/complete/email",
		"/users/change_email",
		"/users/change_email/complete/email",
		"/users/delete",
		"/users/logout",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			writeProtoResponse(w, &protos.EmptyResponse{})
		})
	}

	mux.HandleFunc("/users/recovery/validate/email", func(w http.ResponseWriter, r *http.Request) {
		writeProtoResponse(w, &protos.ValidateRecoveryCodeResponse{Valid: true})
	})

	// Pro server endpoints
	mux.HandleFunc("/user-create", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, UserDataResponse{
			BaseResponse: &protos.BaseResponse{},
			LoginResponse_UserData: &protos.LoginResponse_UserData{
				UserId: 123,
				Token:  "test-token",
			},
		})
	})

	mux.HandleFunc("/user-data", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, UserDataResponse{
			BaseResponse: &protos.BaseResponse{},
			LoginResponse_UserData: &protos.LoginResponse_UserData{
				UserId: 123,
				Token:  "test-token",
			},
		})
	})

	mux.HandleFunc("/user-link-remove", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, map[string]any{
			"userID": 123,
			"token":  "token",
		})
	})

	mux.HandleFunc("/referral-attach", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code string `json:"code"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		state.referralAttachV1Code = body.Code
		writeJSONResponse(w, &protos.BaseResponse{Error: state.referralAttachV1Error})
	})

	mux.HandleFunc("/referral-attach-v2", func(w http.ResponseWriter, r *http.Request) {
		state.referralAttachV2Channel = r.URL.Query().Get("distributionChannel")
		var body struct {
			Code string `json:"code"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		state.referralAttachV2Code = body.Code
		writeJSONResponse(w, ReferralAttachResponse{
			BaseResponse: &protos.BaseResponse{Error: state.referralAttachV2Error},
			Code:         body.Code,
			DiscountPct:  20,
			Type:         "affiliate",
			Plans:        []*protos.Plan{{Id: "1y-usd-10", Description: "Pro Plan"}},
		})
	})

	// Subscription endpoints
	mux.HandleFunc("/plans-v5", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, SubscriptionPlans{
			BaseResponse: &protos.BaseResponse{},
			Plans:        []*protos.Plan{{Id: "1y-usd-10", Description: "Pro Plan"}},
		})
	})

	mux.HandleFunc("/subscription-payment-redirect", func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		state.subscriptionPaymentRedirectIdempotencyKey = values.Get("idempotencyKey")
		_, state.subscriptionPaymentRedirectHasIdempotencyKey = values["idempotencyKey"]
		state.subscriptionPaymentRedirectCouponCode = values.Get("couponCode")
		_, state.subscriptionPaymentRedirectHasCouponCode = values["couponCode"]
		writeJSONResponse(w, map[string]string{"redirect": "https://example.com/redirect"})
	})

	mux.HandleFunc("/payment-redirect", func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		state.paymentRedirectAppVersion = r.Header.Get(common.AppVersionHeader)
		state.paymentRedirectLibraryVersion = r.Header.Get(common.VersionHeader)
		state.paymentRedirectIdempotencyKey = values.Get("idempotencyKey")
		_, state.paymentRedirectHasIdempotencyKey = values["idempotencyKey"]
		state.paymentRedirectCouponCode = values.Get("couponCode")
		_, state.paymentRedirectHasCouponCode = values["couponCode"]
		resp := state.paymentRedirectResponse
		if resp == nil {
			resp = map[string]string{"redirect": "https://example.com/redirect"}
		}
		writeJSONResponse(w, resp)
	})

	mux.HandleFunc("/stripe-subscription", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		state.stripeSubscriptionCouponCode, state.stripeSubscriptionHasCouponCode = body["couponCode"]
		writeJSONResponse(w, SubscriptionResponse{
			CustomerID:     "cus_123",
			SubscriptionID: "sub_123",
			ClientSecret:   "secret",
		})
	})

	mux.HandleFunc("/purchase-apple-subscription-v2", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, VerifySubscriptionResponse{
			Status:         "active",
			SubscriptionID: "sub_1234567890",
		})
	})

	restoreHandler := func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, RestoreSubscriptionResponse{
			Status:          "active",
			ActualUserID:    1234,
			ActualUserToken: "token",
		})
	}
	mux.HandleFunc("/restore-googleplay-subscription", restoreHandler)
	mux.HandleFunc("/restore-apple-subscription", restoreHandler)

	mux.HandleFunc("/purchase", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, PurchaseResponse{
			BaseResponse:  &protos.BaseResponse{},
			PaymentStatus: "completed",
		})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, state
}

func newTestClient(t *testing.T) (*Client, *testServer) {
	ts, state := newTestServer(t)
	// Legacy settings discovery reads these roots; empty ones keep the host's own files out.
	for _, v := range []string{"HOME", "XDG_CONFIG_HOME", "APPDATA", "PUBLIC"} {
		t.Setenv(v, t.TempDir())
	}
	settings.Reset()
	dataDir := t.TempDir()
	require.NoError(t, settings.InitSettings(dataDir))
	require.NoError(t, settings.Set(settings.DataPathKey, dataDir))
	t.Cleanup(settings.Reset)
	return &Client{
		httpClient: ts.Client(),
		logger:     log.NoOpLogger(),
		proURL:     ts.URL,
		authURL:    ts.URL,
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
	}, state
}

// failSettingsSaves makes settings saves fail until the returned func restores the data directory.
func failSettingsSaves(t *testing.T) (restore func()) {
	t.Helper()
	dir := settings.GetString(settings.DataPathKey)
	aside := dir + ".aside"
	require.NoError(t, os.Rename(dir, aside))
	return func() { require.NoError(t, os.Rename(aside, dir)) }
}

// newTestClientWithSRP creates a test client and pre-registers an email/password on the mock server.
func newTestClientWithSRP(t *testing.T, email, password string) (*Client, *testServer) {
	ac, state := newTestClient(t)

	salt, err := generateSalt()
	require.NoError(t, err)

	encKey, err := generateEncryptedKey(password, email, salt)
	require.NoError(t, err)

	srpClient := srp.NewSRPClient(srp.KnownGroups[group], encKey, nil)
	verifierKey, err := srpClient.Verifier()
	require.NoError(t, err)

	state.salt[email] = salt
	state.verifier = verifierKey.Bytes()
	ac.salt = salt

	return ac, state
}

// useProServer points a's pro-server requests at handler for the rest of the test.
func useProServer(t *testing.T, a *Client, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	a.proURL = server.URL
}

func serveJSON(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { writeJSONResponse(w, v) }
}

// heldJSON writes v only once release is closed, closing requested when the request arrives.
func heldJSON(v any) (handler http.HandlerFunc, requested, release chan struct{}) {
	requested, release = make(chan struct{}), make(chan struct{})
	return func(w http.ResponseWriter, r *http.Request) {
		close(requested)
		<-release
		writeJSONResponse(w, v)
	}, requested, release
}

func setOAuthFlags(t *testing.T) {
	t.Helper()
	require.NoError(t, settings.Patch(settings.Settings{
		settings.OAuthLoginKey:    true,
		settings.OAuthProviderKey: "google",
	}))
}

func TestSignUp(t *testing.T) {
	ac, _ := newTestClient(t)
	settings.Set(settings.TokenKey, "test-token")
	settings.Set(settings.UserIDKey, "123")
	salt, signupResponse, err := ac.SignUp(context.Background(), "test@example.com", "password")
	assert.NoError(t, err)
	assert.NotNil(t, salt)
	assert.NotNil(t, signupResponse)
}

func TestSignUpKeepsSaltWhenSaveFails(t *testing.T) {
	ac, _ := newTestClient(t)
	require.NoError(t, settings.Patch(settings.Settings{settings.TokenKey: "t", settings.UserIDKey: "123"}))
	ac.salt = []byte("account-salt")
	defer failSettingsSaves(t)()

	_, _, err := ac.SignUp(context.Background(), "new@example.com", "password")

	assert.ErrorContains(t, err, "saving signup identity")
	assert.Equal(t, []byte("account-salt"), ac.getSaltCached())
}

func TestSignupEmailResendCode(t *testing.T) {
	ac, _ := newTestClient(t)
	ac.salt = []byte("salt")
	err := ac.SignupEmailResendCode(context.Background(), "test@example.com")
	assert.NoError(t, err)
}

func TestSignupEmailConfirmation(t *testing.T) {
	ac, _ := newTestClient(t)
	err := ac.SignupEmailConfirmation(context.Background(), "test@example.com", "code")
	assert.NoError(t, err)
}

func TestLogin(t *testing.T) {
	email := "test@example.com"
	ac, _ := newTestClientWithSRP(t, email, "password")
	// Clear cached salt to test the full flow (getSalt → srpLogin)
	ac.salt = nil
	_, err := ac.Login(context.Background(), email, "password")
	assert.NoError(t, err)
}

func TestDeviceLimitedLoginStoresDevices(t *testing.T) {
	for _, tt := range []struct {
		name     string
		existing *UserData
	}{
		{name: "fresh identity"},
		{name: "existing identity", existing: &UserData{
			LegacyID:       7,
			LegacyToken:    "tok",
			Devices:        []*protos.LoginResponse_Device{{Id: "stale", Name: "Old"}},
			LegacyUserData: &protos.LoginResponse_UserData{UserId: 7, UserLevel: "free"},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, server := newTestClientWithSRP(t, "test@example.com", "password")
			if tt.existing != nil {
				a.setData(tt.existing, nil)
			}
			userDataBefore := settings.Exists(settings.UserDataKey)
			server.loginResponse = &UserData{
				Devices: []*protos.LoginResponse_Device{
					{Id: "phone", Name: "Phone"},
					{Id: "laptop", Name: "Laptop"},
				},
			}

			_, err := a.Login(context.Background(), "test@example.com", "password")
			require.NoError(t, err)

			devices, err := settings.Devices()
			require.NoError(t, err)
			assert.Equal(t, []settings.Device{{ID: "phone", Name: "Phone"}, {ID: "laptop", Name: "Laptop"}}, devices)
			assert.Equal(t, userDataBefore, settings.Exists(settings.UserDataKey),
				"a device-limited login is not a successful login")
		})
	}
}
func TestLogout(t *testing.T) {
	ac, _ := newTestClient(t)
	settings.Set(settings.DeviceIDKey, "deviceId")
	_, err := ac.Logout(context.Background(), "test@example.com")
	assert.NoError(t, err)
}

func TestLogoutKeepsAccountWhenSaveFails(t *testing.T) {
	ac, _ := newTestClient(t)
	require.NoError(t, ac.setData(&UserData{LegacyID: 7, LegacyToken: "tok"}, nil))
	defer failSettingsSaves(t)()

	_, err := ac.Logout(context.Background(), "test@example.com")

	assert.ErrorContains(t, err, "logged out on the server")
	assert.Equal(t, int64(7), settings.GetInt64(settings.UserIDKey))
	assert.Equal(t, "tok", settings.GetString(settings.TokenKey))
}

func TestLoginKeepsAccountWhenSaveFails(t *testing.T) {
	ac, server := newTestClientWithSRP(t, "test@example.com", "password")
	require.NoError(t, ac.setData(&UserData{LegacyID: 7, LegacyToken: "tok"}, nil))
	server.loginResponse = &UserData{
		LegacyID:       8,
		LegacyToken:    "new",
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 8, UserLevel: "pro"},
	}
	defer failSettingsSaves(t)()

	_, err := ac.Login(context.Background(), "test@example.com", "password")

	assert.ErrorContains(t, err, "saving login")
	assert.Equal(t, int64(7), settings.GetInt64(settings.UserIDKey))
	assert.Equal(t, "tok", settings.GetString(settings.TokenKey))
	assert.False(t, settings.IsPro())
}

func TestLogoutOmitsCredentials(t *testing.T) {
	const (
		legacyToken = "legacy-token-value"
		jwtToken    = "jwt-token-value"
	)
	ac, _ := newTestClient(t)
	settings.Set(settings.DeviceIDKey, "deviceId")
	settings.Set(settings.TokenKey, legacyToken)
	settings.Set(settings.JwtTokenKey, jwtToken)

	var buf bytes.Buffer
	ac.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	_, err := ac.Logout(context.Background(), "test@example.com")
	require.NoError(t, err)
	for _, secret := range []string{legacyToken, jwtToken, "test@example.com"} {
		assert.NotContains(t, buf.String(), secret, "credential or PII leaked into the log")
	}
	assert.Contains(t, buf.String(), "deviceId=deviceId", "logout log dropped the device ID")
}

func TestStartRecoveryByEmail(t *testing.T) {
	ac, _ := newTestClient(t)
	err := ac.StartRecoveryByEmail(context.Background(), "test@example.com")
	assert.NoError(t, err)
}

func TestCompleteRecoveryByEmail(t *testing.T) {
	ac, _ := newTestClient(t)
	err := ac.CompleteRecoveryByEmail(context.Background(), "test@example.com", "newPassword", "code")
	assert.NoError(t, err)
}

func TestRecoverySalt(t *testing.T) {
	completeRecovery := func(t *testing.T, a *Client, email string) error {
		t.Helper()
		return a.CompleteRecoveryByEmail(context.Background(), email, "new-password", "code")
	}

	t.Run("current account caches and saves the new salt", func(t *testing.T) {
		a, _ := newTestClient(t)
		require.NoError(t, settings.Set(settings.EmailKey, "me@example.com"))
		a.salt = []byte("old-salt")

		require.NoError(t, completeRecovery(t, a, "Me@example.com"))

		saved, err := readSalt(a.saltPath)
		require.NoError(t, err)
		assert.NotEqual(t, []byte("old-salt"), a.getSaltCached())
		assert.Equal(t, a.getSaltCached(), saved)
	})

	t.Run("a failed save still replaces the cached salt", func(t *testing.T) {
		a, _ := newTestClient(t)
		require.NoError(t, settings.Set(settings.EmailKey, "me@example.com"))
		a.salt = []byte("old-salt")
		a.saltPath = filepath.Join(t.TempDir(), "missing", saltFileName)

		assert.Error(t, completeRecovery(t, a, "me@example.com"))
		assert.NotEqual(t, []byte("old-salt"), a.getSaltCached())
	})

	t.Run("another account's salt is not stored", func(t *testing.T) {
		a, _ := newTestClient(t)
		require.NoError(t, settings.Set(settings.EmailKey, "me@example.com"))
		a.salt = []byte("my-salt")
		require.NoError(t, writeSalt([]byte("my-salt"), a.saltPath))

		require.NoError(t, completeRecovery(t, a, "other@example.com"))

		assert.Equal(t, []byte("my-salt"), a.getSaltCached())
		saved, err := os.ReadFile(a.saltPath)
		require.NoError(t, err)
		assert.Equal(t, []byte("my-salt"), saved)
	})
}

func TestValidateEmailRecoveryCode(t *testing.T) {
	ac, _ := newTestClient(t)
	err := ac.ValidateEmailRecoveryCode(context.Background(), "test@example.com", "code")
	assert.NoError(t, err)
}

func TestVerifyPassword(t *testing.T) {
	email := "test@example.com"
	ac, _ := newTestClientWithSRP(t, email, "password")
	proof, err := ac.VerifyPassword(context.Background(), email, "password")
	assert.NoError(t, err)
	assert.NotEmpty(t, proof)
}

func TestVerifyPassword_WrongPassword(t *testing.T) {
	email := "test@example.com"
	ac, _ := newTestClientWithSRP(t, email, "password")
	proof, err := ac.VerifyPassword(context.Background(), email, "wrong-password")
	assert.Error(t, err)
	assert.Nil(t, proof)
}

func TestStartChangeEmail(t *testing.T) {
	email := "test@example.com"
	ac, _ := newTestClientWithSRP(t, email, "password")
	settings.Set(settings.EmailKey, email)
	err := ac.StartChangeEmail(context.Background(), "new@example.com", "password")
	assert.NoError(t, err)
}

func TestCompleteChangeEmail(t *testing.T) {
	ac, _ := newTestClient(t)
	settings.Set(settings.EmailKey, "old@example.com")
	err := ac.CompleteChangeEmail(context.Background(), "new@example.com", "password", "code")
	assert.NoError(t, err)
}

func TestChangeEmailDiscardsStale(t *testing.T) {
	ac, _ := newTestClient(t)
	require.NoError(t, ac.setData(&UserData{
		LegacyID:       1,
		LegacyToken:    "a",
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 1, Email: "old@example.com"},
	}, nil))
	handler, requested, release := heldJSON(UserDataResponse{
		LoginResponse_UserData: &protos.LoginResponse_UserData{UserId: 1, Token: "a", Email: "old@example.com"},
	})
	useProServer(t, ac, handler)
	errc := make(chan error, 1)
	go func() {
		_, err := ac.FetchUserData(context.Background())
		errc <- err
	}()
	<-requested

	require.NoError(t, ac.CompleteChangeEmail(context.Background(), "new@example.com", "password", "code"))
	close(release)

	assert.ErrorIs(t, <-errc, ErrAccountChanged)
	assert.Equal(t, "new@example.com", settings.GetString(settings.EmailKey))
}

func TestChangeEmailSurvivesUpdate(t *testing.T) {
	ac, _ := newTestClient(t)
	require.NoError(t, ac.setData(&UserData{
		LegacyID:       1,
		LegacyToken:    "a",
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 1, Email: "old@example.com"},
	}, nil))
	ac.salt = []byte("old-salt")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, ac.PatchAccountSettings(settings.Settings{settings.UserLevelKey: "pro"}))
		writeProtoResponse(w, &protos.EmptyResponse{})
	}))
	defer server.Close()
	ac.authURL = server.URL

	require.NoError(t, ac.CompleteChangeEmail(context.Background(), "new@example.com", "password", "code"))

	assert.Equal(t, "new@example.com", settings.GetString(settings.EmailKey))
	assert.NotEqual(t, []byte("old-salt"), ac.getSaltCached())
}

func TestChangeEmailSkipsOtherAccount(t *testing.T) {
	ac, _ := newTestClient(t)
	require.NoError(t, ac.setData(&UserData{
		LegacyID:       1,
		LegacyToken:    "a",
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 1, Email: "old@example.com"},
	}, nil))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, ac.PatchAccountSettings(settings.Settings{settings.UserIDKey: int64(2), settings.TokenKey: "b"}))
		writeProtoResponse(w, &protos.EmptyResponse{})
	}))
	defer server.Close()
	ac.authURL = server.URL

	assert.Error(t, ac.CompleteChangeEmail(context.Background(), "new@example.com", "password", "code"))

	assert.Equal(t, int64(2), settings.GetInt64(settings.UserIDKey))
	assert.Empty(t, settings.GetString(settings.EmailKey))
}

func TestDeleteAccount(t *testing.T) {
	email := "test@example.com"
	ac, _ := newTestClientWithSRP(t, email, "password")
	settings.Set(settings.DeviceIDKey, "deviceId")
	_, err := ac.DeleteAccount(context.Background(), email, "password")
	assert.NoError(t, err)
}

func TestDeleteAccountKeepsAccountWhenSaveFails(t *testing.T) {
	email := "test@example.com"
	ac, _ := newTestClientWithSRP(t, email, "password")
	require.NoError(t, ac.setData(&UserData{LegacyID: 7, LegacyToken: "tok"}, nil))
	defer failSettingsSaves(t)()

	_, err := ac.DeleteAccount(context.Background(), email, "password")

	assert.ErrorContains(t, err, "account deleted on the server")
	assert.Equal(t, int64(7), settings.GetInt64(settings.UserIDKey))
}

func TestOAuthLoginUrl(t *testing.T) {
	ac, _ := newTestClient(t)
	url, err := ac.OAuthLoginURL(context.Background(), "google")
	assert.NoError(t, err)
	assert.NotEmpty(t, url)
}

func TestOAuthLoginCallback(t *testing.T) {
	ac, _ := newTestClient(t)
	settings.Set(settings.DeviceIDKey, "deviceId")
	ac.setData(&UserData{
		LegacyID: 456, Token: "old-jwt",
		Devices:        []*protos.LoginResponse_Device{{Id: "old-device"}},
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 456},
	}, nil)

	// Mock JWT with unverified signature — decodeJWT uses ParseUnverified so this succeeds.
	mockToken := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJlbWFpbCI6InRlc3RAZXhhbXBsZS5jb20iLCJsZWdhY3lfdXNlcl9pZCI6MTIzNDUsImxlZ2FjeV90b2tlbiI6InRlc3QtdG9rZW4ifQ.test"

	data, err := ac.OAuthLoginCallback(context.Background(), mockToken)
	assert.NoError(t, err)
	assert.NotEmpty(t, data)
	assert.Equal(t, mockToken, settings.GetString(settings.JwtTokenKey))
	devices, err := settings.Devices()
	require.NoError(t, err)
	assert.Empty(t, devices)
}

func TestOAuthLoginDiscardedOnSwitch(t *testing.T) {
	ac, _ := newTestClient(t)
	useProServer(t, ac, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, ac.PatchAccountSettings(settings.Settings{settings.UserIDKey: int64(99), settings.TokenKey: "other"}))
		writeJSONResponse(w, UserDataResponse{
			LoginResponse_UserData: &protos.LoginResponse_UserData{UserId: 12345, Token: "test-token"},
		})
	}))
	mockToken := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJlbWFpbCI6InRlc3RAZXhhbXBsZS5jb20iLCJsZWdhY3lfdXNlcl9pZCI6MTIzNDUsImxlZ2FjeV90b2tlbiI6InRlc3QtdG9rZW4ifQ.test"

	_, err := ac.OAuthLoginCallback(context.Background(), mockToken)

	assert.ErrorIs(t, err, ErrAccountChanged)
	assert.Equal(t, int64(99), settings.GetInt64(settings.UserIDKey))
	assert.Empty(t, settings.GetString(settings.JwtTokenKey))
	assert.False(t, settings.GetBool(settings.OAuthLoginKey))
}

func TestOAuthLoginSavesAccount(t *testing.T) {
	ac, _ := newTestClient(t)
	require.NoError(t, ac.setData(&UserData{
		LegacyID: 1, LegacyToken: "old-token", Token: "old-jwt",
		LegacyUserData: &protos.LoginResponse_UserData{UserId: 1, Email: "old@example.com"},
	}, nil))
	require.NoError(t, ac.storeSalt([]byte("old-salt")))
	require.NoError(t, settings.Set(settings.DeviceIDKey, "device"))
	require.NoError(t, settings.Set(settings.OAuthProviderKey, "google"))
	before := currentAccount()
	generation := ac.identityGeneration()
	useProServer(t, ac, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "2", r.Header.Get(common.UserIDHeader))
		assert.Equal(t, "oauth-token", r.Header.Get(common.ProTokenHeader))
		assert.Equal(t, "device", r.Header.Get(common.DeviceIDHeader))
		assert.Equal(t, before, currentAccount())
		assert.Equal(t, []byte("old-salt"), ac.getSaltCached())
		writeJSONResponse(w, UserDataResponse{
			LoginResponse_UserData: &protos.LoginResponse_UserData{UserId: 2, UserLevel: "pro"},
		})
	}))
	token := mockJWT(t, map[string]any{
		"legacy_user_id": 2, "legacy_token": "oauth-token", "email": "new@example.com",
	})

	user, err := ac.OAuthLoginCallback(context.Background(), token)

	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, generation+1, ac.identityGeneration())
	assert.Equal(t, "new@example.com", user.Id)
	assert.True(t, user.EmailConfirmed)
	assert.Equal(t, token, user.Token)
	assert.Equal(t, "oauth-token", user.LegacyToken)
	assert.Equal(t, "new@example.com", user.LegacyUserData.Email)
	assert.Equal(t, "device", user.LegacyUserData.DeviceID)
	require.NoError(t, settings.Reload())
	assert.Equal(t, int64(2), settings.GetInt64(settings.UserIDKey))
	assert.Equal(t, "oauth-token", settings.GetString(settings.TokenKey))
	assert.Equal(t, token, settings.GetString(settings.JwtTokenKey))
	assert.Equal(t, "new@example.com", settings.GetString(settings.EmailKey))
	assert.True(t, settings.GetBool(settings.OAuthLoginKey))
	assert.Equal(t, "google", settings.GetString(settings.OAuthProviderKey))
	assert.True(t, settings.IsPro())
	var cached UserData
	require.NoError(t, settings.GetStruct(settings.UserDataKey, &cached))
	assert.True(t, proto.Equal(user, &cached))
	assert.Nil(t, ac.getSaltCached())
}

func TestOAuthLoginFailureKeepsAccount(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		body     string
		failSave bool
	}{
		{name: "request", status: http.StatusServiceUnavailable, body: "unavailable"},
		{name: "json", status: http.StatusOK, body: "invalid"},
		{name: "missing user data", status: http.StatusOK, body: "{}"},
		{name: "missing identity", status: http.StatusOK, body: `{"email":"new@example.com"}`},
		{name: "save", status: http.StatusOK, body: `{"userId":2,"token":"oauth-token"}`, failSave: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ac, _ := newTestClient(t)
			require.NoError(t, ac.setData(&UserData{
				LegacyID: 1, LegacyToken: "old-token", Token: "old-jwt",
				LegacyUserData: &protos.LoginResponse_UserData{UserId: 1, Email: "old@example.com"},
			}, nil))
			require.NoError(t, ac.storeSalt([]byte("old-salt")))
			before := settings.GetAll()
			useProServer(t, ac, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			if tt.failSave {
				defer failSettingsSaves(t)()
			}
			token := mockJWT(t, map[string]any{"legacy_user_id": 2, "legacy_token": "oauth-token"})

			user, err := ac.OAuthLoginCallback(context.Background(), token)

			require.Error(t, err)
			assert.Nil(t, user)
			assert.Equal(t, before, settings.GetAll())
			assert.Equal(t, []byte("old-salt"), ac.getSaltCached())
		})
	}
}

func TestOAuthLoginCallback_InvalidToken(t *testing.T) {
	ac, _ := newTestClient(t)

	_, err := ac.OAuthLoginCallback(context.Background(), "invalid-token")
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.Contains(t, err.Error(), "error decoding JWT")
}

func TestOAuthLoginRejectsNoIdentity(t *testing.T) {
	for _, claims := range []map[string]any{
		{"legacy_token": "oauth-token"},
		{"legacy_user_id": 2},
	} {
		ac, _ := newTestClient(t)
		useProServer(t, ac, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("no request should be sent for a token without an account identity")
		}))

		_, err := ac.OAuthLoginCallback(context.Background(), mockJWT(t, claims))

		assert.ErrorIs(t, err, ErrInvalidToken)
	}
}

// mockJWT builds an unsigned JWT from the given claims; decodeJWT uses
// ParseUnverified, so the fake signature is never checked.
func mockJWT(t *testing.T, claims map[string]any) string {
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".test"
}

func TestOAuthDeviceLimitCallback(t *testing.T) {
	ac, _ := newTestClient(t)

	token := mockJWT(t, map[string]any{
		"email":          "test@example.com",
		"legacy_user_id": 12345,
		"legacy_token":   "test-token",
	})
	err := ac.OAuthDeviceLimitCallback(context.Background(), token)
	assert.NoError(t, err)
	assert.EqualValues(t, 12345, settings.GetInt64(settings.UserIDKey))
	assert.Equal(t, "test-token", settings.GetString(settings.TokenKey))
}

func TestOAuthDeviceLimitCallback_InvalidToken(t *testing.T) {
	ac, _ := newTestClient(t)

	err := ac.OAuthDeviceLimitCallback(context.Background(), "invalid-token")
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.Contains(t, err.Error(), "error decoding JWT")
}

func TestOAuthDeviceLimitCallback_MissingIdentity(t *testing.T) {
	ac, _ := newTestClient(t)

	token := mockJWT(t, map[string]any{"email": "test@example.com"})
	err := ac.OAuthDeviceLimitCallback(context.Background(), token)
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.Contains(t, err.Error(), "missing the account identity")
}

func TestReferralAttach_EmptyChannelUsesV1(t *testing.T) {
	ac, ts := newTestClient(t)
	resp, err := ac.ReferralAttach(context.Background(), "AFF123", "")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "AFF123", ts.referralAttachV1Code)
	assert.Empty(t, ts.referralAttachV2Code)
}

func TestReferralAttach_V1Error(t *testing.T) {
	ac, ts := newTestClient(t)
	ts.referralAttachV1Error = "invalid code"
	resp, err := ac.ReferralAttach(context.Background(), "BAD", "")
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "invalid code")
}

func TestReferralAttach_ChannelUsesV2(t *testing.T) {
	ac, ts := newTestClient(t)
	resp, err := ac.ReferralAttach(context.Background(), "AFF123", "store")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "AFF123", ts.referralAttachV2Code)
	assert.Equal(t, "store", ts.referralAttachV2Channel)
	assert.Empty(t, ts.referralAttachV1Code)
	assert.Equal(t, "AFF123", resp.Code)
	assert.Equal(t, 20, resp.DiscountPct)
	assert.Equal(t, "affiliate", resp.Type)
	assert.NotEmpty(t, resp.Plans)
}

func TestReferralAttach_V2Error(t *testing.T) {
	ac, ts := newTestClient(t)
	ts.referralAttachV2Error = "invalid code"
	resp, err := ac.ReferralAttach(context.Background(), "BAD", "store")
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "invalid code")
}

func TestOAuthFlagsCleared(t *testing.T) {
	t.Run("ClearUser", func(t *testing.T) {
		a, _ := newTestClient(t)
		setOAuthFlags(t)
		a.ClearUser()
		assert.False(t, settings.GetBool(settings.OAuthLoginKey))
		assert.Empty(t, settings.GetString(settings.OAuthProviderKey))
	})

	t.Run("SignUp", func(t *testing.T) {
		a, _ := newTestClient(t)
		require.NoError(t, settings.Patch(settings.Settings{settings.TokenKey: "t", settings.UserIDKey: "123"}))
		setOAuthFlags(t)
		_, _, err := a.SignUp(context.Background(), "new@example.com", "password")
		require.NoError(t, err)
		assert.False(t, settings.GetBool(settings.OAuthLoginKey))
		assert.Empty(t, settings.GetString(settings.OAuthProviderKey))
	})
}
