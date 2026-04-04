// Package settings provides a simple interface for storing and retrieving user settings.
package settings

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"

	"github.com/getlantern/radiance/common/atomicfile"
)

type _key string

const (
	// Keys for various settings.
	// General settings keys.
	DataPathKey    _key = "data_path"    // string
	LogPathKey     _key = "log_path"     // string
	LogLevelKey    _key = "log_level"    // string
	CountryCodeKey _key = "country_code" // string
	LocaleKey      _key = "locale"       // string
	DeviceIDKey    _key = "device_id"    // string/int

	// Application behavior related keys.
	TelemetryKey           _key = "telemetry_enabled"     // bool
	ConfigFetchDisabledKey _key = "config_fetch_disabled" // bool

	// User account related keys.
	EmailKey         _key = "email"          // string
	UserIDKey        _key = "user_id"        // string
	UserLevelKey     _key = "user_level"     // string
	TokenKey         _key = "token"          // string
	JwtTokenKey      _key = "jwt_token"      // string
	DevicesKey       _key = "devices"        // []Device
	UserDataKey      _key = "user_data"      // [account.UserData]
	OAuthLoginKey    _key = "oauth_login"    // bool
	OAuthProviderKey _key = "oauth_provider" // string (e.g. "google", "apple", "email")

	// VPN related keys.
	SmartRoutingKey   _key = "smart_routing"   // bool
	SplitTunnelKey    _key = "split_tunnel"    // bool
	AdBlockKey        _key = "ad_block"        // bool
	AutoConnectKey    _key = "auto_connect"    // bool
	SelectedServerKey _key = "selected_server" // [servers.Server] Server.Options is not stored

	PreferredLocationKey _key = "preferred_location" // [common.PreferredLocation]

	settingsFileName = "settings.json"
)

var ErrNotExist = errors.New("key does not exist")

func (k _key) String() string { return string(k) }

type settings struct {
	k           *koanf.Koanf
	initialized bool
	filePath    string
	mu          sync.Mutex
}

var k = &settings{
	k: koanf.New("."),
}

func init() {
	// set default values.
	k.k.Set(LocaleKey.String(), "fa-IR")
	k.k.Set(UserLevelKey.String(), "free")
}

// InitSettings initializes the config for user settings.
func InitSettings(fileDir string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.initialized {
		return nil
	}
	if err := os.MkdirAll(fileDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %v", err)
	}
	k.filePath = filepath.Join(fileDir, settingsFileName)
	switch err := loadSettings(k.filePath); {
	case errors.Is(err, fs.ErrNotExist):
		slog.Warn("settings file not found", "path", k.filePath) // file may not have been created yet
		return save()
	case err != nil:
		return fmt.Errorf("loading settings: %w", err)
	}
	k.initialized = true
	return nil
}

func loadSettings(path string) error {
	contents, err := atomicfile.ReadFile(path)
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}
	kk := koanf.New(".")
	if err := kk.Load(rawbytes.Provider(contents), json.Parser()); err != nil {
		return fmt.Errorf("parsing settings: %w", err)
	}
	k.k = kk
	return nil
}

func Get(key _key) any {
	return k.k.Get(key.String())
}

func GetString(key _key) string {
	return k.k.String(key.String())
}

func GetBool(key _key) bool {
	return k.k.Bool(key.String())
}

func GetInt(key _key) int {
	return k.k.Int(key.String())
}

func GetInt64(key _key) int64 {
	return k.k.Int64(key.String())
}

func GetFloat64(key _key) float64 {
	return k.k.Float64(key.String())
}

func GetStringSlice(key _key) []string {
	return k.k.Strings(key.String())
}

func GetDuration(key _key) time.Duration {
	return k.k.Duration(key.String())
}

func GetStruct(key _key, out any) error {
	return k.k.Unmarshal(key.String(), out)
}

func Exists(key _key) bool {
	return k.k.Exists(key.String())
}

func Set(key _key, value any) error {
	err := k.k.Set(key.String(), value)
	if err != nil {
		return fmt.Errorf("could not set key %s: %w", key, err)
	}
	return save()
}

func Clear(key _key) {
	k.k.Delete(key.String())
}

type Settings map[_key]any

func (s Settings) Diff(s2 Settings) Settings {
	diff := make(Settings)
	for k, v1 := range s {
		if v2, ok := s2[k]; !ok || v1 != v2 {
			diff[k] = v1
		}
	}
	return diff
}

func GetAll() Settings {
	s := make(Settings)
	for key, value := range k.k.All() {
		s[_key(key)] = value
	}
	return s
}

func GetAllFor(keys ..._key) Settings {
	if len(keys) == 0 {
		return GetAll()
	}
	s := make(Settings)
	for _, key := range keys {
		s[key] = k.k.Get(key.String())
	}
	return s
}

// Patch takes a map of settings to update and applies them all at once.
func Patch(updates Settings) error {
	for key, value := range updates {
		if err := k.k.Set(_key(key).String(), value); err != nil {
			return fmt.Errorf("could not set key %s: %w", key, err)
		}
	}
	return save()
}

func save() error {
	out, err := k.k.Marshal(json.Parser())
	if err != nil {
		return fmt.Errorf("could not marshal koanf file: %w", err)
	}

	err = atomicfile.WriteFile(k.filePath, out, 0644)
	if err != nil {
		return fmt.Errorf("could not write koanf file: %w", err)
	}
	return nil
}

// Reset clears the current settings in memory primarily for testing purposes.
func Reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.k = koanf.New(".")
	k.initialized = false
}

func IsPro() bool {
	return strings.ToLower(GetString(UserLevelKey)) == "pro"
}

// Device is a machine registered to a user account (e.g. an Android phone or a Windows desktop).
type Device struct {
	ID   string
	Name string
}

func Devices() ([]Device, error) {
	devices := []Device{}
	err := GetStruct(DevicesKey, &devices)
	return devices, err
}
