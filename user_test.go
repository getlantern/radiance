package radiance

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/1Password/srp"
	"github.com/getlantern/radiance/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetUser(t *testing.T) {
	r := &Radiance{}
	u := r.GetUser()
	assert.NotNil(t, u)
}

func TestDevices(t *testing.T) {
	u := &User{
		userData: &user.LoginResponse{
			Devices: []*user.LoginResponse_Device{
				{Name: "Device1", Id: "1"},
				{Name: "Device2", Id: "2"},
			},
		},
	}
	devices, err := u.Devices()
	assert.NoError(t, err)
	assert.Len(t, devices, 2)
}

func TestSubscription(t *testing.T) {
	u := &User{}
	_, err := u.Subscription()
	assert.Error(t, err)
}

func TestDataCapInfo(t *testing.T) {
	u := &User{}
	_, err := u.DataCapInfo()
	assert.Error(t, err)
}

func TestSignUp(t *testing.T) {
	u := &User{
		authClient: &mockAuthClient{},
	}
	err := u.SignUp(context.Background(), "test@example.com", "password")
	assert.NoError(t, err)
}

func TestSignupEmailResendCode(t *testing.T) {
	u := &User{
		salt:       []byte("salt"),
		authClient: &mockAuthClient{},
	}
	err := u.SignupEmailResendCode(context.Background(), "test@example.com")
	assert.NoError(t, err)
}

func TestSignupEmailConfirmation(t *testing.T) {
	u := &User{
		authClient: &mockAuthClient{},
	}
	err := u.SignupEmailConfirmation(context.Background(), "test@example.com", "code")
	assert.NoError(t, err)
}

func TestLogin(t *testing.T) {
	u := &User{
		authClient: &mockAuthClient{},
	}
	err := u.Login(context.Background(), "test@example.com", "password", "deviceId")
	assert.NoError(t, err)
}

func TestLogout(t *testing.T) {
	u := &User{
		userData:   &user.LoginResponse{Id: "test@example.com"},
		authClient: &mockAuthClient{},
		deviceId:   "deviceId",
	}
	err := u.Logout(context.Background())
	assert.NoError(t, err)
}

func TestStartRecoveryByEmail(t *testing.T) {
	u := &User{
		authClient: &mockAuthClient{},
	}
	err := u.StartRecoveryByEmail(context.Background(), "test@example.com")
	assert.NoError(t, err)
}

func TestCompleteRecoveryByEmail(t *testing.T) {
	u := &User{
		authClient: &mockAuthClient{},
	}
	err := u.CompleteRecoveryByEmail(context.Background(), "test@example.com", "newPassword", "code")
	assert.NoError(t, err)
}

func TestValidateEmailRecoveryCode(t *testing.T) {
	u := &User{
		authClient: &mockAuthClient{},
	}
	err := u.ValidateEmailRecoveryCode(context.Background(), "test@example.com", "code")
	assert.NoError(t, err)
}

func TestStartChangeEmail(t *testing.T) {
	email := "test@example.com"
	authClient := mockAuthClientNew(t, email, "password")
	u := &User{
		userData:   &user.LoginResponse{Id: email},
		authClient: authClient,
		salt:       authClient.salt[email],
	}
	err := u.StartChangeEmail(context.Background(), "new@example.com", "password")
	assert.NoError(t, err)
}

func TestCompleteChangeEmail(t *testing.T) {
	u := &User{
		userData:   &user.LoginResponse{Id: "test@example.com"},
		authClient: &mockAuthClient{},
	}
	err := u.CompleteChangeEmail(context.Background(), "new@example.com", "password", "code")
	assert.NoError(t, err)
}

func TestDeleteAccount(t *testing.T) {
	email := "test@example.com"
	authClient := mockAuthClientNew(t, email, "password")
	u := &User{
		userData:   &user.LoginResponse{Id: "test@example.com"},
		authClient: authClient,
		deviceId:   "deviceId",
		salt:       authClient.salt[email],
	}
	err := u.DeleteAccount(context.Background(), "password")
	assert.NoError(t, err)
}

// Mock implementation of AuthClient for testing purposes
type mockAuthClient struct {
	cache    map[string]string
	salt     map[string][]byte
	verifier []byte
}

func mockAuthClientNew(t *testing.T, email, password string) *mockAuthClient {
	salt, err := user.GenerateSalt()
	require.NoError(t, err)

	srpClient := srp.NewSRPClient(srp.KnownGroups[group], user.GenerateEncryptedKey(password, email, salt), nil)
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

func (m *mockAuthClient) SignupEmailResendCode(ctx context.Context, req *user.SignupEmailResendRequest) error {
	return nil
}

func (m *mockAuthClient) SignupEmailConfirmation(ctx context.Context, req *user.ConfirmSignupRequest) error {
	return nil
}

func (m *mockAuthClient) GetSalt(ctx context.Context, email string) (*user.GetSaltResponse, error) {
	return &user.GetSaltResponse{Salt: []byte("salt")}, nil
}

func (m *mockAuthClient) Login(ctx context.Context, email, password, deviceId string, salt []byte) (*user.LoginResponse, error) {
	return &user.LoginResponse{}, nil
}

func (m *mockAuthClient) SignOut(ctx context.Context, req *user.LogoutRequest) error {
	return nil
}

func (m *mockAuthClient) StartRecoveryByEmail(ctx context.Context, req *user.StartRecoveryByEmailRequest) error {
	return nil
}

func (m *mockAuthClient) CompleteRecoveryByEmail(ctx context.Context, req *user.CompleteRecoveryByEmailRequest) error {
	return nil
}

func (m *mockAuthClient) ValidateEmailRecoveryCode(ctx context.Context, req *user.ValidateRecoveryCodeRequest) (*user.ValidateRecoveryCodeResponse, error) {
	return &user.ValidateRecoveryCodeResponse{Valid: true}, nil
}

func (m *mockAuthClient) ChangeEmail(ctx context.Context, req *user.ChangeEmailRequest) error {
	return nil
}

func (m *mockAuthClient) CompleteChangeEmail(ctx context.Context, req *user.CompleteChangeEmailRequest) error {
	return nil
}

func (m *mockAuthClient) DeleteAccount(ctx context.Context, req *user.DeleteUserRequest) error {
	return nil
}

func (m *mockAuthClient) LoginPrepare(ctx context.Context, req *user.PrepareRequest) (*user.PrepareResponse, error) {
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
	return &user.PrepareResponse{B: B.Bytes(), Proof: proof}, nil
}
