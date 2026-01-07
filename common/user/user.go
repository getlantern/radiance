package user

// this file contains the user info interface and the methods to read and write user data
// use this across the app to read and write user data in sync
import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
)

const userDataFileName = ".userData"

// userInfo is a struct that implements the UserInfo interface
// it contains the device ID, user data, data directory, and locale
type userInfo struct {
	deviceID    string
	data        *protos.LoginResponse
	locale      string
	countryCode string
	filepath    string
	mu          sync.RWMutex
}

// NewUserConfig creates a new UserInfo object
func NewUserConfig(deviceID, dataDir, locale string) common.UserInfo {
	path := filepath.Join(dataDir, userDataFileName)
	u, err := load(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("Failed to load user data -- presumably the first run", "path", path, "error", err)
		} else {
			slog.Warn("Failed to load user data -- potential issue", "path", path, "error", err)
		}
	}
	if u == nil {
		u = &userInfo{}
	}
	u.deviceID = deviceID
	u.filepath = path
	u.locale = locale
	save(u, path)

	events.SubscribeOnce(func(evt config.NewConfigEvent) {
		if evt.New != nil && evt.New.ConfigResponse.Country != "" {
			u.countryCode = evt.New.ConfigResponse.Country
			save(u, path)
		}
	})
	return u
}

func (u *userInfo) DeviceID() string {
	return u.deviceID
}

func (u *userInfo) LegacyID() int64 {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.data != nil {
		return u.data.LegacyID
	}
	return 0
}

func (u *userInfo) LegacyToken() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
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

func (u *userInfo) CountryCode() string {
	return u.countryCode
}

// AccountType returns the account type of the user (e.g., "free", "pro")
func (u *userInfo) AccountType() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.data == nil || u.data.LegacyUserData == nil {
		return ""
	}
	typ := u.data.LegacyUserData.UserLevel
	if typ == "" {
		return "free"
	}
	return typ
}

func (u *userInfo) IsPro() bool {
	return strings.ToLower(u.AccountType()) == "pro"
}

func (u *userInfo) SetData(data *protos.LoginResponse) error {
	u.mu.Lock()
	old := &userInfo{
		deviceID:    u.deviceID,
		data:        u.data,
		locale:      u.locale,
		countryCode: u.countryCode,
		filepath:    u.filepath,
	}
	u.data = data
	u.mu.Unlock()
	if data != nil && !proto.Equal(old.data, data) {
		events.Emit(common.UserChangeEvent{Old: old, New: u})
	}
	return save(u, u.filepath)
}

// GetUserData reads user data from file
func (u *userInfo) GetData() (*protos.LoginResponse, error) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.data, nil
}

type _user struct {
	DeviceID string         `json:"device_id"`
	Data     *loginResponse `json:"data,omitempty"`
	Locale   string         `json:"locale"`
	Country  string         `json:"country_code,omitempty"`
}

func (u *userInfo) MarshalJSON() ([]byte, error) {
	u.mu.RLock()
	c := &_user{
		DeviceID: u.deviceID,
		Data:     (*loginResponse)(u.data),
		Locale:   u.locale,
		Country:  u.countryCode,
	}
	u.mu.RUnlock()
	return json.Marshal(c)
}

func (u *userInfo) UnmarshalJSON(data []byte) error {
	var c _user
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	u.deviceID = c.DeviceID
	u.data = (*protos.LoginResponse)(c.Data)
	u.locale = c.Locale
	u.countryCode = c.Country
	return nil
}

type loginResponse protos.LoginResponse

func (lr *loginResponse) MarshalJSON() ([]byte, error) {
	return protojson.Marshal((*protos.LoginResponse)(lr))
}

func (lr *loginResponse) UnmarshalJSON(data []byte) error {
	return protojson.Unmarshal(data, (*protos.LoginResponse)(lr))
}

// Save user to file
func save(user *userInfo, path string) error {
	slog.Debug("Saving user data", "path", path)
	bytes, err := json.Marshal(user)
	if err != nil {
		return fmt.Errorf("failed to marshal user data: %w", err)
	}
	if err := atomicfile.WriteFile(path, bytes, 0600); err != nil {
		return fmt.Errorf("failed to write user data: %w", err)
	}
	return nil
}

func load(path string) (*userInfo, error) {
	data, err := atomicfile.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil // File does not exist. could be first run
	}
	if err != nil {
		return nil, err
	}
	var u userInfo
	if err := json.Unmarshal(data, &u); err == nil {
		return &u, nil
	} else {
		// TODO: remove in a future release
		// Try to unmarshal as legacy protobuf format
		slog.Info("Failed to unmarshal user data as JSON, trying legacy protobuf format", "error", err)
		var loginData protos.LoginResponse
		if perr := proto.Unmarshal(data, &loginData); perr == nil {
			slog.Info("Migrating user data to new JSON format")
			return &userInfo{
				data: &loginData,
			}, nil
		}
		return nil, fmt.Errorf("failed to unmarshal user data: %w", err)
	}
}

func Load(dataPath string) (*userInfo, error) {
	path := filepath.Join(dataPath, userDataFileName)
	return load(path)
}
