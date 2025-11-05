package common

// this file contains the user info interface and the methods to read and write user data
// use this acrosss the app to read and write user data in sync
import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/events"
)

const userDataFileName = ".userData"

// UserInfo is an interface that defines the methods for user configuration
type UserInfo interface {
	DeviceID() string
	LegacyID() int64
	LegacyToken() string
	SetData(*protos.LoginResponse) error
	GetData() (*protos.LoginResponse, error)
	Locale() string
	SetLocale(string)
}

// userInfo is a struct that implements the UserInfo interface
// it contains the device ID, user data, data directory, and locale
type userInfo struct {
	deviceID string
	data     *protos.LoginResponse
	dataPath string
	locale   string
}

// NewUserConfig creates a new UserInfo object
func NewUserConfig(deviceID, dataDir, locale string) UserInfo {
	path := filepath.Join(dataDir, userDataFileName)
	data, err := load(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("Failed to load user data -- presumably the first run", "path", path, "error", err)
		} else {
			slog.Warn("Failed to load user data -- potential issue", "path", path, "error", err)
		}
	}
	u := &userInfo{
		deviceID: deviceID,
		data:     data,
		dataPath: path,
		locale:   locale,
	}
	return u
}

func (u *userInfo) DeviceID() string {
	return u.deviceID
}

func (u *userInfo) LegacyID() int64 {
	if u.data != nil {
		return u.data.LegacyID
	}
	return 0
}

func (u *userInfo) LegacyToken() string {
	if u.data != nil {
		return u.data.LegacyToken
	}
	return ""
}

func (u *userInfo) Locale() string {
	return u.locale
}

func (u *userInfo) SetLocale(locale string) {
	u.locale = locale
}

type UserChangeEvent struct {
	Old *protos.LoginResponse
	New *protos.LoginResponse
}

func (u *userInfo) SetData(data *protos.LoginResponse) error {
	old := u.data
	u.data = data
	if data != nil && !proto.Equal(old, data) {
		events.Emit(UserChangeEvent{Old: old, New: data})
	}
	return save(data, u.dataPath)
}

// GetUserData reads user data from file
func (u *userInfo) GetData() (*protos.LoginResponse, error) {
	// We have already read the data from file, so we can return it directly
	if u.data != nil {
		return u.data, nil
	}
	data, err := os.ReadFile(u.dataPath)
	if err != nil {
		return nil, err
	}
	var resp protos.LoginResponse
	if err := proto.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Save user data to file
func save(data *protos.LoginResponse, path string) error {
	slog.Debug("Saving user data", "path", path)
	bytes, err := proto.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal user data: %w", err)
	}
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		return fmt.Errorf("failed to write user data: %w", err)
	}
	return nil
}

func load(path string) (*protos.LoginResponse, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil // File does not exist. could be first run
	}
	if err != nil {
		return nil, err
	}
	var resp protos.LoginResponse
	if err := proto.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user data: %w", err)
	}
	return &resp, nil
}
