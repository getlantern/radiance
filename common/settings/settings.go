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
	"sync/atomic"
	"time"

	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/internal"
)

// Keys for various settings.
const (
	CountryCodeKey   = "country_code"
	LocaleKey        = "locale"
	DeviceIDKey      = "device_id"
	DataPathKey      = "data_path"
	LogPathKey       = "log_path"
	EmailKey         = "email"
	UserLevelKey     = "user_level"
	TokenKey         = "token"
	UserIDKey        = "user_id"
	DevicesKey       = "devices"
	LogLevelKey      = "log_level"
	LoginResponseKey = "login_response"
	SmartRoutingKey  = "smart_routing"
	AdBlockKey       = "ad_block"
	UnboundedKey     = "unbounded"
	filePathKey      = "file_path"

	settingsFileName = "local.json"
)

type settings struct {
	k           *koanf.Koanf
	readOnly    atomic.Bool
	initialized bool
	watcher     *internal.FileWatcher
	mu          sync.Mutex
}

var k = &settings{
	k: koanf.New("."),
}

var ErrReadOnly = errors.New("read-only")

// InitSettings initializes the config for user settings, which can be used by both the tunnel process and
// the main application process to read user preferences like locale.
func InitSettings(fileDir string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.initialized {
		return nil
	}
	if err := initialize(fileDir); err != nil {
		return fmt.Errorf("initializing settings: %w", err)
	}
	k.initialized = true
	return nil
}

func initialize(fileDir string) error {
	k.k = koanf.New(".")
	if err := os.MkdirAll(fileDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %v", err)
	}
	filePath := filepath.Join(fileDir, settingsFileName)
	switch err := loadSettings(filePath); {
	case errors.Is(err, fs.ErrNotExist):
		slog.Warn("settings file not found", "path", filePath) // file may not have been created yet
		if err := setDefaults(filePath); err != nil {
			return fmt.Errorf("setting default settings: %w", err)
		}
		return save()
	case err != nil:
		return fmt.Errorf("loading settings: %w", err)
	}
	return nil
}

func setDefaults(filePath string) error {
	// We need to set the file path first because the save function reads it as soon as we set any key.
	if err := k.k.Set(filePathKey, filePath); err != nil {
		return fmt.Errorf("failed to set file path: %w", err)
	}
	if err := k.k.Set(LocaleKey, "fa-IR"); err != nil {
		return fmt.Errorf("failed to set default locale: %w", err)
	}
	if err := k.k.Set(UserLevelKey, "free"); err != nil {
		return fmt.Errorf("failed to set default user level: %w", err)
	}
	return nil
}

func loadSettings(path string) error {
	contents, err := atomicfile.ReadFile(path)
	if err != nil {
		return fmt.Errorf("loading settings (read-only): %w", err)
	}
	kk := koanf.New(".")
	if err := kk.Load(rawbytes.Provider(contents), json.Parser()); err != nil {
		return fmt.Errorf("parsing settings: %w", err)
	}
	k.k = kk
	return nil
}

func SetReadOnly(readOnly bool) {
	k.readOnly.Store(readOnly)
}

func StartWatching() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.initialized {
		return errors.New("settings not initialized")
	}
	if k.watcher != nil {
		return errors.New("settings file watcher already started")
	}

	path := k.k.String(filePathKey)
	watcher := internal.NewFileWatcher(path, func() {
		if err := loadSettings(path); err != nil {
			slog.Error("reloading settings file", "error", err)
		}
	})
	if err := watcher.Start(); err != nil {
		return fmt.Errorf("starting settings file watcher: %w", err)
	}
	k.watcher = watcher
	// reload settings once at start in case there were changes before we started watching
	if err := loadSettings(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// StopWatching stops watching the settings file for changes. This is only relevant in read-only mode.
func StopWatching() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.watcher != nil {
		k.watcher.Close()
		k.watcher = nil
	}
}

func Get(key string) any {
	return k.k.Get(key)
}

func GetString(key string) string {
	return k.k.String(key)
}

func GetBool(key string) bool {
	return k.k.Bool(key)
}

func GetInt(key string) int {
	return k.k.Int(key)
}

func GetInt64(key string) int64 {
	return k.k.Int64(key)
}

func GetFloat64(key string) float64 {
	return k.k.Float64(key)
}

func GetStringSlice(key string) []string {
	return k.k.Strings(key)
}

func GetDuration(key string) time.Duration {
	return k.k.Duration(key)
}

func GetStruct(key string, out any) error {
	return k.k.Unmarshal(key, out)
}

func Set(key string, value any) error {
	if k.readOnly.Load() {
		return ErrReadOnly
	}
	err := k.k.Set(key, value)
	if err != nil {
		return fmt.Errorf("could not set key %s: %w", key, err)
	}
	return save()
}

func save() error {
	if k.readOnly.Load() {
		return ErrReadOnly
	}
	if GetString(filePathKey) == "" {
		return errors.New("settings file path is not set")
	}
	out, err := k.k.Marshal(json.Parser())
	if err != nil {
		return fmt.Errorf("could not marshal koanf file: %w", err)
	}

	err = atomicfile.WriteFile(GetString(filePathKey), out, 0644)
	if err != nil {
		return fmt.Errorf("could not write koanf file: %w", err)
	}
	return nil
}

// Reset clears the current settings in memory primarily for testing purposes.
func Reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.readOnly.Load() {
		if k.watcher != nil {
			k.watcher.Close()
			k.watcher = nil
		}
		k.k = koanf.New(".")
		k.initialized = false
	}
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

type UserChangeEvent struct {
	events.Event
}
