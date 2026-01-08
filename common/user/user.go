package user

// this file contains the user info interface and the methods to read and write user data
// use this across the app to read and write user data in sync
import (
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
)

// userInfo is a struct that implements the UserInfo interface
// it contains the device ID, user data, data directory, and locale
type userInfo struct {
	mu sync.RWMutex
}

// NewUserConfig creates a new UserInfo object
func NewUserConfig(deviceID, dataDir, locale string) common.UserInfo {
	u := &userInfo{}
	if err := settings.Set(settings.DeviceIDKey, deviceID); err != nil {
		slog.Error("failed to set device ID in settings", "error", err)
	}
	if err := settings.Set(settings.DataPathKey, dataDir); err != nil {
		slog.Error("failed to set data path in settings", "error", err)
	}
	if err := settings.Set(settings.LocaleKey, locale); err != nil {
		slog.Error("failed to set locale in settings", "error", err)
	}

	var sub *events.Subscription[config.NewConfigEvent]
	sub = events.Subscribe(func(evt config.NewConfigEvent) {
		if evt.New != nil && evt.New.ConfigResponse.Country != "" {
			if err := settings.Set(settings.CountryCodeKey, evt.New.ConfigResponse.Country); err != nil {
				slog.Error("failed to set country code in settings", "error", err)
			}
			slog.Info("Set country code from config response", "country_code", evt.New.ConfigResponse.Country)
			events.Unsubscribe(sub)
		}
	})
	return u
}

func (u *userInfo) DeviceID() string {
	return settings.GetString(settings.DeviceIDKey)
}

func (u *userInfo) LegacyID() int64 {
	data, err := u.GetData()
	if err != nil {
		slog.Info("failed to get login data from settings", "error", err)
		return 0
	}
	if data != nil {
		return data.LegacyID
	}
	return 0
}

func (u *userInfo) LegacyToken() string {
	data, err := u.GetData()
	if err != nil {
		slog.Info("failed to get login data from settings", "error", err)
		return ""
	}
	if data != nil {
		return data.LegacyToken
	}
	return ""
}

func (u *userInfo) Locale() string {
	return settings.GetString(settings.LocaleKey)
}

func (u *userInfo) SetLocale(locale string) {
	if err := settings.Set(settings.LocaleKey, locale); err != nil {
		slog.Error("failed to set locale in settings", "error", err)
	}
}

func (u *userInfo) CountryCode() string {
	return settings.GetString(settings.CountryCodeKey)
}

// AccountType returns the account type of the user (e.g., "free", "pro")
func (u *userInfo) AccountType() string {
	data, err := u.GetData()
	if err != nil {
		slog.Info("failed to get login data from settings", "error", err)
		return "free"
	}
	if data == nil || data.LegacyUserData == nil {
		return ""
	}
	typ := data.LegacyUserData.UserLevel
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
	defer u.mu.Unlock()
	oldData, err := u.getDataNoLock()
	if err != nil {
		slog.Warn("failed to get login data from settings", "error", err)
	}
	if err = settings.Set(settings.LoginDataKey, data); err != nil {
		slog.Error("failed to set login data in settings", "error", err)
	}

	if data != nil && !proto.Equal(oldData, data) {
		events.Emit(common.UserChangeEvent{Old: oldData, New: data})
	}
	return err
}

// GetUserData reads user data from file
func (u *userInfo) GetData() (*protos.LoginResponse, error) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.getDataNoLock()
}

// getDataNoLock reads user data from file without acquiring locks
func (u *userInfo) getDataNoLock() (*protos.LoginResponse, error) {
	data := &protos.LoginResponse{}
	err := settings.GetStruct(settings.LoginDataKey, data)
	if err != nil {
		slog.Warn("failed to get login data from settings", "error", err)
		return nil, err
	}
	return data, nil
}
