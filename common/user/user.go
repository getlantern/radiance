package user

// this file contains the user info interface and the methods to read and write user data
// use this across the app to read and write user data in sync
import (
	"log/slog"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
)

// userInfo is a struct that implements the UserInfo interface
// it contains the device ID, user data, data directory, and locale
type userInfo struct {
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
