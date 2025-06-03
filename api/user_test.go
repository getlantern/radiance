package api

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/1Password/srp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
)

func TestSignUp(t *testing.T) {
	ac := &APIClient{
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		authClient: &mockAuthClient{},
		userInfo:   &mockUserInfo{},
	}
	err := ac.SignUp(context.Background(), "test@example.com", "password")
	assert.NoError(t, err)
}

func TestSignupEmailResendCode(t *testing.T) {
	ac := &APIClient{
		salt:       []byte("salt"),
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		authClient: &mockAuthClient{},
	}
	err := ac.SignupEmailResendCode(context.Background(), "test@example.com")
	assert.NoError(t, err)
}

func TestSignupEmailConfirmation(t *testing.T) {
	ac := &APIClient{
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		authClient: &mockAuthClient{},
	}
	err := ac.SignupEmailConfirmation(context.Background(), "test@example.com", "code")
	assert.NoError(t, err)
}

func TestLogin(t *testing.T) {
	ac := &APIClient{
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		authClient: &mockAuthClient{},
		userInfo:   &mockUserInfo{},
	}
	_, err := ac.Login(context.Background(), "test@example.com", "password", "deviceId")
	assert.NoError(t, err)
}

func TestLogout(t *testing.T) {
	ac := &APIClient{
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		userData:   &protos.LoginResponse{Id: "test@example.com"},
		userInfo:   &mockUserInfo{},
		authClient: &mockAuthClient{},
		deviceID:   "deviceId",
	}
	err := ac.Logout(context.Background(), "test@example.com")
	assert.NoError(t, err)
}

func TestStartRecoveryByEmail(t *testing.T) {
	ac := &APIClient{
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		authClient: &mockAuthClient{},
		userInfo:   &mockUserInfo{},
	}
	err := ac.StartRecoveryByEmail(context.Background(), "test@example.com")
	assert.NoError(t, err)
}

func TestCompleteRecoveryByEmail(t *testing.T) {
	ac := &APIClient{
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		authClient: &mockAuthClient{},
		userInfo:   &mockUserInfo{},
	}
	err := ac.CompleteRecoveryByEmail(context.Background(), "test@example.com", "newPassword", "code")
	assert.NoError(t, err)
}

func TestValidateEmailRecoveryCode(t *testing.T) {
	ac := &APIClient{
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		authClient: &mockAuthClient{},
		userInfo:   &mockUserInfo{},
	}
	err := ac.ValidateEmailRecoveryCode(context.Background(), "test@example.com", "code")
	assert.NoError(t, err)
}

func TestStartChangeEmail(t *testing.T) {
	email := "test@example.com"
	authClient := mockAuthClientNew(t, email, "password")
	ac := &APIClient{
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		userData:   &protos.LoginResponse{Id: email},
		authClient: authClient,
		salt:       authClient.salt[email],
	}
	err := ac.StartChangeEmail(context.Background(), "new@example.com", "password")
	assert.NoError(t, err)
}

func TestCompleteChangeEmail(t *testing.T) {
	ac := &APIClient{
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		userData:   &protos.LoginResponse{Id: "test@example.com"},
		authClient: &mockAuthClient{},
		userInfo:   &mockUserInfo{},
	}
	err := ac.CompleteChangeEmail(context.Background(), "new@example.com", "password", "code")
	assert.NoError(t, err)
}

func TestDeleteAccount(t *testing.T) {
	email := "test@example.com"
	authClient := mockAuthClientNew(t, email, "password")
	ac := &APIClient{
		saltPath:   filepath.Join(t.TempDir(), saltFileName),
		userData:   &protos.LoginResponse{Id: "test@example.com"},
		authClient: authClient,
		deviceID:   "deviceId",
		salt:       authClient.salt[email],
		userInfo:   &mockUserInfo{},
	}
	err := ac.DeleteAccount(context.Background(), "","password")
	assert.NoError(t, err)
}

func TestOAuthLoginUrl(t *testing.T) {
	ac := &APIClient{
		saltPath: filepath.Join(t.TempDir(), saltFileName),
		userInfo: userInfo(t.TempDir()),
	}
	url, err := ac.OAuthLoginUrl(context.Background(), "google")
	assert.NoError(t, err)
	assert.NotEmpty(t, url)
}

// Mock implementation of AuthClient for testing purposes
type mockAuthClient struct {
	cache    map[string]string
	salt     map[string][]byte
	verifier []byte
}

func mockAuthClientNew(t *testing.T, email, password string) *mockAuthClient {
	salt, err := generateSalt()
	require.NoError(t, err)

	encKey, err := generateEncryptedKey(password, email, salt)
	require.NoError(t, err)

	srpClient := srp.NewSRPClient(srp.KnownGroups[group], encKey, nil)
	verifierKey, err := srpClient.Verifier()
	require.NoError(t, err)

	m := &mockAuthClient{
		salt:     map[string][]byte{email: salt},
		verifier: verifierKey.Bytes(),
		cache:    make(map[string]string),
	}
	return m
}

func (m *mockAuthClient) SignUp(ctx context.Context, email, password string) ([]byte, error) {
	return []byte("salt"), nil
}

func (m *mockAuthClient) SignupEmailResendCode(ctx context.Context, req *protos.SignupEmailResendRequest) error {
	return nil
}

func (m *mockAuthClient) SignupEmailConfirmation(ctx context.Context, req *protos.ConfirmSignupRequest) error {
	return nil
}

func (m *mockAuthClient) GetSalt(ctx context.Context, email string) (*protos.GetSaltResponse, error) {
	return &protos.GetSaltResponse{Salt: []byte("salt")}, nil
}

func (m *mockAuthClient) Login(ctx context.Context, email, password, deviceId string, salt []byte) (*protos.LoginResponse, error) {
	return &protos.LoginResponse{}, nil
}

func (m *mockAuthClient) SignOut(ctx context.Context, req *protos.LogoutRequest) error {
	return nil
}

func (m *mockAuthClient) StartRecoveryByEmail(ctx context.Context, req *protos.StartRecoveryByEmailRequest) error {
	return nil
}

func (m *mockAuthClient) CompleteRecoveryByEmail(ctx context.Context, req *protos.CompleteRecoveryByEmailRequest) error {
	return nil
}

func (m *mockAuthClient) ValidateEmailRecoveryCode(ctx context.Context, req *protos.ValidateRecoveryCodeRequest) (*protos.ValidateRecoveryCodeResponse, error) {
	return &protos.ValidateRecoveryCodeResponse{Valid: true}, nil
}

func (m *mockAuthClient) ChangeEmail(ctx context.Context, req *protos.ChangeEmailRequest) error {
	return nil
}

func (m *mockAuthClient) CompleteChangeEmail(ctx context.Context, req *protos.CompleteChangeEmailRequest) error {
	return nil
}

func (m *mockAuthClient) DeleteAccount(ctx context.Context, req *protos.DeleteUserRequest) error {
	return nil
}

func (m *mockAuthClient) LoginPrepare(ctx context.Context, req *protos.PrepareRequest) (*protos.PrepareResponse, error) {
	A := big.NewInt(0).SetBytes(req.A)
	verifier := big.NewInt(0).SetBytes(m.verifier)

	server := srp.NewSRPServer(srp.KnownGroups[srp.RFC5054Group3072], verifier, nil)
	if err := server.SetOthersPublic(A); err != nil {
		return nil, err
	}
	B := server.EphemeralPublic()
	if B == nil {
		return nil, errors.New("cannot generate B")
	}
	if _, err := server.Key(); err != nil {
		return nil, errors.New("cannot generate key")
	}
	proof, err := server.M(m.salt[req.Email], req.Email)
	if err != nil {
		return nil, errors.New("cannot generate Proof")
	}
	state, err := server.MarshalBinary()
	if err != nil {
		return nil, err
	}
	m.cache[req.Email] = hex.EncodeToString(state)
	return &protos.PrepareResponse{B: B.Bytes(), Proof: proof}, nil
}

var _ common.UserInfo = (*mockUserInfo)(nil)

// Mock implementation of User config for testing purposes
type mockUserInfo struct{}

func (m *mockUserInfo) GetData() (*protos.LoginResponse, error) {
	return &protos.LoginResponse{}, nil
}

func (m *mockUserInfo) SetData(userData *protos.LoginResponse) error { return nil }
func (m *mockUserInfo) DeviceID() string                             { return "deviceId" }
func (m *mockUserInfo) LegacyID() int64                              { return 1 }
func (m *mockUserInfo) LegacyToken() string                          { return "legacyToken" }
func (m *mockUserInfo) Locale() string                               { return "en-US" }
func (m *mockUserInfo) SetLocale(locale string)                      {}
