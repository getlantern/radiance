package user

// this file contains the user info interface and the methods to read and write user data
// use this across the app to read and write user data in sync
import (
	"log/slog"
	"strings"
	"sync"

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

func (u *userInfo) LegacyToken() string {
	return settings.GetString(settings.TokenKey)
}

func (u *userInfo) CountryCode() string {
	return settings.GetString(settings.CountryCodeKey)
}

// AccountType returns the account type of the user (e.g., "free", "pro")
func (u *userInfo) AccountType() string {
	return settings.GetString(settings.UserLevelKey)
}

func (u *userInfo) IsPro() bool {
	return strings.ToLower(u.AccountType()) == "pro"
}

func (u *userInfo) GetEmail() string {
	return settings.GetString(settings.EmailKey)
}

func (u *userInfo) SetEmail(email string) error {
	return settings.Set(settings.EmailKey, email)
}

type Devices struct {
	Devices []common.Device
}

func (u *userInfo) Devices() ([]common.Device, error) {
	d := &Devices{
		Devices: []common.Device{},
	}
	err := settings.GetStruct(settings.DevicesKey, d)
	return d.Devices, err
}

func (u *userInfo) SetData(data *protos.LoginResponse) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var changed bool
	if data == nil || data.LegacyUserData == nil {
		slog.Info("no user data to set")
		return
	}

	if data.LegacyUserData.UserLevel != "" {
		oldUserLevel := settings.GetString(settings.UserLevelKey)
		changed = changed || oldUserLevel != data.LegacyUserData.UserLevel
		if err := settings.Set(settings.UserLevelKey, data.LegacyUserData.UserLevel); err != nil {
			slog.Error("failed to set user level in settings", "error", err)
		}
	}
	if data.LegacyUserData.Email != "" {
		oldEmail := settings.GetString(settings.EmailKey)
		changed = changed || oldEmail != "" && oldEmail != data.LegacyUserData.Email
		if err := settings.Set(settings.EmailKey, data.LegacyUserData.Email); err != nil {
			slog.Error("failed to set email in settings", "error", err)
		}
	}
	if data.LegacyID != 0 {
		oldUserID := settings.GetInt64(settings.UserIDKey)
		changed = changed || oldUserID != 0 && oldUserID != data.LegacyID
		if err := settings.Set(settings.UserIDKey, data.LegacyID); err != nil {
			slog.Error("failed to set user ID in settings", "error", err)
		}
	}

	devices := []common.Device{}
	for _, d := range data.Devices {
		devices = append(devices, common.Device{
			Name: d.Name,
			ID:   d.Id,
		})
	}
	d := &Devices{
		Devices: devices,
	}
	if err := settings.Set(settings.DevicesKey, d); err != nil {
		slog.Error("failed to set devices in settings", "error", err)
	}

	if changed {
		events.Emit(common.UserChangeEvent{})
	}
}
