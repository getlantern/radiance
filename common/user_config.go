package common

// this file contains the user config interface and the methods to read and write user data
// use this acrosss the app to read and write user data in sync
import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/getlantern/radiance/user/protos"
	"google.golang.org/protobuf/proto"
)

// UserConfig is an interface that defines the methods for user configuration
type UserConfig interface {
	DeviceID() string
	LegacyID() int64
	LegacyToken() string
	Save(*protos.LoginResponse) error
	GetUserData() (*protos.LoginResponse, error)
	ReadSalt() ([]byte, error)
	WriteSalt([]byte) error
}

type userConfig struct {
	deviceID string
	resp     *protos.LoginResponse
	dataDir  string
}

var (
	saltLocation     = ".salt" // TODO: we need to think about properly storing data. Right now both configFetcher and this module just dump things in the current directory. Instead there should be a 'data writer' that knows where to put things.
	userDataLocation = ".userData"
	activeConfig     *userConfig
)

func NewUserConfig(deviceID, dataDir string) UserConfig {
	resp, _ := ReadUserData(dataDir)
	u := &userConfig{deviceID: deviceID, resp: resp, dataDir: dataDir}
	activeConfig = u
	return u
}

func (u *userConfig) DeviceID() string {
	return u.deviceID
}

func (u *userConfig) LegacyID() int64 {
	if u.resp != nil {
		return u.resp.LegacyID
	}
	return 0
}

func (u *userConfig) LegacyToken() string {
	if u.resp != nil {
		return u.resp.LegacyToken
	}
	return ""
}

func (u *userConfig) Save(data *protos.LoginResponse) error {
	savePath := filepath.Join(u.dataDir, userDataLocation)
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

func (u *userConfig) GetUserData() (*protos.LoginResponse, error) {
	readPath := filepath.Join(u.dataDir, userDataLocation)
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
func ReadUserData(dataDir string) (*protos.LoginResponse, error) {
	readPath := filepath.Join(dataDir, userDataLocation)
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

func WriteUserData(data *protos.LoginResponse) error {
	activeConfig.resp = data
	bytes, err := proto.Marshal(data)
	if err != nil {
		log.Printf("Error marshalling user data: %v", err)
	}
	return os.WriteFile(userDataLocation, bytes, 0600)

}

func (u *userConfig) WriteSalt(salt []byte) error {
	savePath := filepath.Join(u.dataDir, saltLocation)
	return os.WriteFile(savePath, salt, 0600)
}

func (u *userConfig) ReadSalt() ([]byte, error) {
	readPath := filepath.Join(u.dataDir, saltLocation)
	return os.ReadFile(readPath)
}
