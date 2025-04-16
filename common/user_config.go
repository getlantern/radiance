package common

// this file contains the user config interface and the methods to read and write user data
// use this acrosss the app to read and write user data in sync
import (
	"log"
	"os"

	"github.com/getlantern/radiance/user/protos"
	"google.golang.org/protobuf/proto"
)

// UserConfig is an interface that defines the methods for user configuration
type UserConfig interface {
	DeviceID() string
	LegacyID() int64
	LegacyToken() string
}

type userConfig struct {
	deviceID string
	resp     *protos.LoginResponse
}

var (
	saltLocation     = ".salt" // TODO: we need to think about properly storing data. Right now both configFetcher and this module just dump things in the current directory. Instead there should be a 'data writer' that knows where to put things.
	userDataLocation = ".userData"
	activeConfig     *userConfig
)

func NewUserConfig(deviceID string) (UserConfig, error) {
	resp, _ := ReadUserData()
	u := &userConfig{deviceID: deviceID, resp: resp}
	activeConfig = u
	return u, nil
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

func WriteUserData(data *protos.LoginResponse) error {
	activeConfig.resp = data
	bytes, err := proto.Marshal(data)
	if err != nil {
		log.Printf("Error marshalling user data: %v", err)
	}
	return os.WriteFile(userDataLocation, bytes, 0600)

}

func ReadUserData() (*protos.LoginResponse, error) {
	data, err := os.ReadFile(userDataLocation)
	if err != nil {
		return nil, err
	}
	var resp protos.LoginResponse
	if err := proto.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func WriteSalt(salt []byte) error {
	return os.WriteFile(saltLocation, salt, 0600)
}

func ReadSalt() ([]byte, error) {
	return os.ReadFile(saltLocation)
}
