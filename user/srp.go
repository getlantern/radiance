package user

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/1Password/srp"
	"golang.org/x/crypto/pbkdf2"
)

func NewSRPClient(email string, password string, salt []byte) (*srp.SRP, error) {
	if len(salt) == 0 || len(password) == 0 || len(email) == 0 {
		return nil, errors.New("salt, password and email should not be empty")
	}

	lowerCaseEmail := strings.ToLower(email)
	encryptedKey, err := GenerateEncryptedKey(password, lowerCaseEmail, salt)
	if err != nil {
		return nil, fmt.Errorf("failed to generate encrypted key: %w", err)
	}

	return srp.NewSRPClient(srp.KnownGroups[group], encryptedKey, nil), nil
}

// Takes password and email, salt and returns encrypted key
func GenerateEncryptedKey(password string, email string, salt []byte) (*big.Int, error) {
	if len(salt) == 0 || len(password) == 0 || len(email) == 0 {
		return nil, errors.New("salt or password or email is empty")
	}
	lowerCaseEmail := strings.ToLower(email)
	combinedInput := password + lowerCaseEmail
	encryptedKey := pbkdf2.Key([]byte(combinedInput), salt, 4096, 32, sha256.New)
	encryptedKeyBigInt := big.NewInt(0).SetBytes(encryptedKey)
	return encryptedKeyBigInt, nil
}

func GenerateSalt() ([]byte, error) {
	salt := make([]byte, 16)
	if n, err := rand.Read(salt); err != nil {
		return nil, err
	} else if n != 16 {
		return nil, errors.New("failed to generate 16 byte salt")
	}
	return salt, nil
}

func (c *authClient) SignUp(ctx context.Context, email string, password string) ([]byte, error) {
	lowerCaseEmail := strings.ToLower(email)
	salt, err := GenerateSalt()
	if err != nil {
		return nil, err
	}

	srpClient, err := NewSRPClient(lowerCaseEmail, password, salt)
	if err != nil {
		return nil, err
	}
	verifierKey, err := srpClient.Verifier()
	if err != nil {
		return nil, err
	}
	signUpRequestBody := &SignupRequest{
		Email:                 lowerCaseEmail,
		Salt:                  salt,
		Verifier:              verifierKey.Bytes(),
		SkipEmailConfirmation: true,
	}

	if err := c.signUp(ctx, signUpRequestBody); err != nil {
		return nil, err
	}
	return salt, nil
}

// Todo find way to optimize this method
func (c *authClient) Login(ctx context.Context, email string, password string, deviceId string, salt []byte) (*LoginResponse, error) {
	lowerCaseEmail := strings.ToLower(email)

	// Prepare login request body
	client, err := NewSRPClient(lowerCaseEmail, password, salt)
	if err != nil {
		return nil, err
	}
	//Send this key to client
	A := client.EphemeralPublic()
	//Create body
	prepareRequestBody := &PrepareRequest{
		Email: lowerCaseEmail,
		A:     A.Bytes(),
	}

	srpB, err := c.LoginPrepare(ctx, prepareRequestBody)
	if err != nil {
		return nil, err
	}

	// // Once the client receives B from the server Client should check error status here as defense against
	// // a malicious B sent from server
	B := big.NewInt(0).SetBytes(srpB.B)

	if err = client.SetOthersPublic(B); err != nil {
		return nil, err
	}

	// client can now make the session key
	clientKey, err := client.Key()
	if err != nil || clientKey == nil {
		return nil, fmt.Errorf("user_not_found error while generating Client key %w", err)
	}

	// Step 3

	// check if the server proof is valid
	if !client.GoodServerProof(salt, lowerCaseEmail, srpB.Proof) {
		return nil, fmt.Errorf("user_not_found error while checking server proof %w", err)
	}

	clientProof, err := client.ClientProof()
	if err != nil {
		return nil, fmt.Errorf("user_not_found error while generating client proof %w", err)
	}
	loginRequestBody := &LoginRequest{
		Email:    lowerCaseEmail,
		Proof:    clientProof,
		DeviceId: deviceId,
	}
	return c.login(ctx, loginRequestBody)
}
