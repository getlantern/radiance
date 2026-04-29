package account

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/1Password/srp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/account/protos"
	"github.com/getlantern/radiance/common/settings"
)

// testServer holds server-side SRP state for the mock auth server.
type testServer struct {
	salt     map[string][]byte
	verifier []byte
	cache    map[string]string
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
		writeProtoResponse(w, &protos.LoginResponse{
			LegacyUserData: &protos.LoginResponse_UserData{
				DeviceID: "deviceId",
			},
		})
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
		writeJSONResponse(w, LinkResponse{
			BaseResponse: &protos.BaseResponse{},
			UserID:       123,
			ProToken:     "token",
		})
	})

	mux.HandleFunc("/referral-attach", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, protos.BaseResponse{})
	})

	// Subscription endpoints
	mux.HandleFunc("/plans-v5", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, SubscriptionPlans{
			BaseResponse: &protos.BaseResponse{},
			Plans:        []*protos.Plan{{Id: "1y-usd-10", Description: "Pro Plan"}},
		})
	})

	mux.HandleFunc("/subscription-payment-redirect", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, map[string]string{"Redirect": "https://example.com/redirect"})
	})

	mux.HandleFunc("/payment-redirect", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, map[string]string{"Redirect": "https://example.com/redirect"})
	})

	mux.HandleFunc("/stripe-subscription", func(w http.ResponseWriter, r *http.Request) {
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
	settings.InitSettings(t.TempDir())
	t.Cleanup(settings.Reset)
	return &Client{
		httpClient: ts.Client(),
		proURL:     ts.URL,
		authURL:    ts.URL,
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
	}, state
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

func TestSignUp(t *testing.T) {
	ac, _ := newTestClient(t)
	settings.Set(settings.TokenKey, "test-token")
	settings.Set(settings.UserIDKey, "123")
	salt, signupResponse, err := ac.SignUp(context.Background(), "test@example.com", "password")
	assert.NoError(t, err)
	assert.NotNil(t, salt)
	assert.NotNil(t, signupResponse)
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

func TestLogout(t *testing.T) {
	ac, _ := newTestClient(t)
	settings.Set(settings.DeviceIDKey, "deviceId")
	_, err := ac.Logout(context.Background(), "test@example.com")
	assert.NoError(t, err)
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

func TestValidateEmailRecoveryCode(t *testing.T) {
	ac, _ := newTestClient(t)
	err := ac.ValidateEmailRecoveryCode(context.Background(), "test@example.com", "code")
	assert.NoError(t, err)
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

func TestDeleteAccount(t *testing.T) {
	email := "test@example.com"
	ac, _ := newTestClientWithSRP(t, email, "password")
	settings.Set(settings.DeviceIDKey, "deviceId")
	_, err := ac.DeleteAccount(context.Background(), email, "password")
	assert.NoError(t, err)
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

	// Mock JWT with unverified signature — decodeJWT uses ParseUnverified so this succeeds.
	mockToken := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJlbWFpbCI6InRlc3RAZXhhbXBsZS5jb20iLCJsZWdhY3lfdXNlcl9pZCI6MTIzNDUsImxlZ2FjeV90b2tlbiI6InRlc3QtdG9rZW4ifQ.test"

	data, err := ac.OAuthLoginCallback(context.Background(), mockToken)
	assert.NoError(t, err)
	assert.NotEmpty(t, data)
}

func TestOAuthLoginCallback_InvalidToken(t *testing.T) {
	ac, _ := newTestClient(t)

	_, err := ac.OAuthLoginCallback(context.Background(), "invalid-token")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error decoding JWT")
}
