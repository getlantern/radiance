package user

// this file contains the user info interface and the methods to read and write user data
// use this across the app to read and write user data in sync
import (
	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"

	"github.com/spf13/viper"
)

//const userDataFileName = ".userData"

// userInfo is a struct that implements the UserInfo interface
// it contains the device ID, user data, data directory, and locale
type userInfo struct {
	//data *protos.LoginResponse
	/*
		deviceID    string
		data        *protos.LoginResponse
		locale      string
		countryCode string
		dataPath    string
		mu          sync.RWMutex
	*/
}

// NewUserConfig creates a new UserInfo object
func NewUserConfig(deviceID, dataDir, locale string) common.UserInfo {
	viper.Set(common.DeviceIDKey, deviceID)
	viper.Set(common.LocaleKey, locale)
	viper.Set(common.DataDirKey, dataDir)
	viper.WriteConfig()

	var sub *events.Subscription[config.NewConfigEvent]
	sub = events.Subscribe(func(evt config.NewConfigEvent) {
		if evt.New != nil && evt.New.ConfigResponse.Country != "" {
			events.Unsubscribe(sub)
			viper.Set(common.CountryCodeKey, evt.New.ConfigResponse.Country)
			viper.WriteConfig()
		}
	})
	return &userInfo{}
}

func (u *userInfo) DeviceID() string {
	return viper.GetString(common.DeviceIDKey)
}

func (u *userInfo) LegacyID() int64 {
	return viper.GetInt64(common.UserIdKey)
}

func (u *userInfo) LegacyToken() string {
	return viper.GetString(common.TokenKey)
}

func (u *userInfo) Locale() string {
	return viper.GetString(common.LocaleKey)
}
func (u *userInfo) SetLocale(locale string) {
	viper.Set(common.LocaleKey, locale)
	viper.WriteConfig()
}

func (u *userInfo) CountryCode() string {
	return viper.GetString(common.CountryCodeKey)
}

// AccountType returns the account type of the user (e.g., "free", "pro")
func (u *userInfo) AccountType() string {
	return viper.GetString(common.TierKey)
}

func (u *userInfo) IsPro() bool {
	return viper.GetString(common.TierKey) == "pro"
}

func (u *userInfo) SetData(data *protos.LoginResponse) error {
	/*
		viper.Set(common.UserIdKey, data.UserId)
		viper.Set(common.TokenKey, data.Token)
		viper.Set(common.CountryCodeKey, data.CountryCode)
		viper.Set(common.LocaleKey, data.Locale)
		return viper.WriteConfig()
	*/

	u.mu.Lock()
	old := &userInfo{
		deviceID:    u.deviceID,
		data:        u.data,
		locale:      u.locale,
		countryCode: u.countryCode,
		dataPath:    u.dataPath,
	}
	u.data = data
	u.mu.Unlock()
	if data != nil && !json.Equal(old.data, data) {
		events.Emit(common.UserChangeEvent{Old: old, New: u})
	}
	return save(u, u.dataPath)
}

// GetUserData reads user data from file

func (u *userInfo) GetData() (*protos.LoginResponse, error) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.data, nil
}

/*
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
*/

/*
func Load(path string) (*userInfo, error) {
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
*/
