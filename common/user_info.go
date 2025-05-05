package common

// this file contains the user info interface and the methods to read and write user data
// use this acrosss the app to read and write user data in sync
import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/getlantern/radiance/api/protos"
	"google.golang.org/protobuf/proto"
)

// UserInfo is an interface that defines the methods for user configuration
type UserInfo interface {
	DeviceID() string
	LegacyID() int64
	LegacyToken() string
	Save(*protos.LoginResponse) error
	GetUserData() (*protos.LoginResponse, error)
	ReadSalt() ([]byte, error)
	WriteSalt([]byte) error
	Locale() string
}

// userInfo is a struct that implements the UserInfo interface
// it contains the device ID, user data, data directory, and locale
type userInfo struct {
	deviceID string
	resp     *protos.LoginResponse
	dataDir  string
	locale   string
}

// append file name to location path
var (
	saltFileName     = ".salt"
	userDataFileName = ".userData"
)

// NewUserConfig creates a new UserInfo object
func NewUserConfig(deviceID, dataDir, locale string) UserInfo {
	resp, _ := ReadUserData(dataDir)
	u := &userInfo{deviceID: deviceID, resp: resp, dataDir: dataDir, locale: locale}
	return u
}

func (u *userInfo) Locale() string {
	return u.locale
}

func (u *userInfo) DeviceID() string {
	return u.deviceID
}

func (u *userInfo) LegacyID() int64 {
	if u.resp != nil {
		return u.resp.LegacyID
	}
	return 0
}

func (u *userInfo) LegacyToken() string {
	if u.resp != nil {
		return u.resp.LegacyToken
	}
	return ""
}

// Save user data to file
func (u *userInfo) Save(data *protos.LoginResponse) error {
	savePath := filepath.Join(u.dataDir, userDataFileName)
	log.Printf("Saving user data to %s", savePath)
	bytes, err := proto.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal user data: %w", err)
	}
	if err := os.WriteFile(savePath, bytes, 0600); err != nil {
		return fmt.Errorf("failed to write user data: %w", err)
	}
	u.resp = data
	return nil
}

// GetUserData reads user data from file
func (u *userInfo) GetUserData() (*protos.LoginResponse, error) {
	//We have already read the data from file, so we can return it directly
	if u.resp != nil {
		return u.resp, nil
	}
	readPath := filepath.Join(u.dataDir, userDataFileName)
	data, err := os.ReadFile(readPath)
	if err != nil {
		return nil, err
	}
	var resp protos.LoginResponse
	if err := proto.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// WriteSalt writes the salt to file
func (u *userInfo) WriteSalt(salt []byte) error {
	savePath := filepath.Join(u.dataDir, saltFileName)
	return os.WriteFile(savePath, salt, 0600)
}

// ReadSalt reads the salt from file
func (u *userInfo) ReadSalt() ([]byte, error) {
	readPath := filepath.Join(u.dataDir, saltFileName)
	return os.ReadFile(readPath)
}

// ReadUserData reads user data from file
// This is a standalone function to read user data from file
func ReadUserData(dataDir string) (*protos.LoginResponse, error) {
	readPath := filepath.Join(dataDir, userDataFileName)
	data, err := os.ReadFile(readPath)
	if err != nil {
		return nil, err
	}
	var resp protos.LoginResponse
	if err := proto.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
