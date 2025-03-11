package user

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"math/big"
	"strings"

	"github.com/1Password/srp"
	"golang.org/x/crypto/pbkdf2"
)

const (
	group = srp.RFC5054Group3072
)

func NewSRPClient(email string, password string, salt []byte) *srp.SRP {
	if len(salt) == 0 || len(password) == 0 || len(email) == 0 {
		log.Errorf("salt, password and email should not be empty %v %v %v", salt, password, email)
		return nil
	}
	// log.Debugf("NewSRPClient email %v password %v salt %v", email, password, salt)
	lowerCaseEmail := strings.ToLower(email)
	encryptedKey := GenerateEncryptedKey(password, lowerCaseEmail, salt)
	// log.Debugf("Encrypted key %v", encryptedKey)
	return srp.NewSRPClient(srp.KnownGroups[group], encryptedKey, nil)
}

// Takes password and email, salt and returns encrypted key
func GenerateEncryptedKey(password string, email string, salt []byte) *big.Int {
	if len(salt) == 0 || len(password) == 0 || len(email) == 0 {
		log.Errorf("slat or password or email is empty %v %v %v", salt, password, email)
		return nil
	}
	lowerCaseEmail := strings.ToLower(email)
	combinedInput := password + lowerCaseEmail
	encryptedKey := pbkdf2.Key([]byte(combinedInput), salt, 4096, 32, sha256.New)
	encryptedKeyBigInt := big.NewInt(0).SetBytes(encryptedKey)
	return encryptedKeyBigInt
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

	srpClient := NewSRPClient(lowerCaseEmail, password, salt)
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
	log.Debugf("Sign up request email %v, salt %v verifier %v verifiter in bytes %v", lowerCaseEmail, salt, verifierKey, verifierKey.Bytes())

	if err := c.signUp(context.Background(), signUpRequestBody); err != nil {
		return nil, err
	}
	return salt, nil
}

// Todo find way to optimize this method
func (c *authClient) Login(ctx context.Context, email string, password string, deviceId string, salt []byte) (*LoginResponse, error) {
	lowerCaseEmail := strings.ToLower(email)

	// Prepare login request body
	client := NewSRPClient(lowerCaseEmail, password, salt)
	//Send this key to client
	A := client.EphemeralPublic()
	//Create body
	prepareRequestBody := &PrepareRequest{
		Email: lowerCaseEmail,
		A:     A.Bytes(),
	}
	log.Debugf("Login prepare request email %v A %v", lowerCaseEmail, A.Bytes())
	srpB, err := c.LoginPrepare(context.Background(), prepareRequestBody)
	if err != nil {
		return nil, err
	}
	log.Debugf("Login prepare response %v", srpB)

	// // Once the client receives B from the server Client should check error status here as defense against
	// // a malicious B sent from server
	B := big.NewInt(0).SetBytes(srpB.B)

	if err = client.SetOthersPublic(B); err != nil {
		log.Errorf("Error while setting srpB %v", err)
		return nil, err
	}

	// client can now make the session key
	clientKey, err := client.Key()
	if err != nil || clientKey == nil {
		return nil, log.Errorf("user_not_found error while generating Client key %v", err)
	}

	// Step 3

	// check if the server proof is valid
	if !client.GoodServerProof(salt, lowerCaseEmail, srpB.Proof) {
		return nil, log.Errorf("user_not_found error while checking server proof%v", err)
	}

	clientProof, err := client.ClientProof()
	if err != nil {
		return nil, log.Errorf("user_not_found error while generating client proof %v", err)
	}
	loginRequestBody := &LoginRequest{
		Email:    lowerCaseEmail,
		Proof:    clientProof,
		DeviceId: deviceId,
	}
	return c.login(context.Background(), loginRequestBody)
}
