package settings

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	CountryCodeKey = "country_code"
	LocaleKey      = "locale"
	DeviceIDKey    = "device_id"
	DataPathKey    = "data_path"
	LogPathKey     = "log_path"
	EmailKey       = "email"
	UserLevelKey   = "user_level"
	TokenKey       = "token"
	UserIDKey      = "user_id"
	DevicesKey     = "devices"
	LogLevelKey    = "log_level"
	filePathKey    = "file_path"

	settingsFileName = "local.json"
)

type settings struct {
	k           *koanf.Koanf
	parser      koanf.Parser
	readOnly    atomic.Bool
	initialized atomic.Bool
	watcher     *internal.FileWatcher
}

var k = &settings{
	k:      koanf.New("."),
	parser: json.Parser(),
}

var ErrReadOnly = errors.New("read-only")

// InitSettings initializes the config for user settings, which can be used by both the tunnel process and
// the main application process to read user preferences like locale.
// func InitSettings() error {
func InitSettings(dataDir string) error {
	if k.initialized.Swap(true) {
		return nil
	}
	if err := initialize(dataDir); err != nil {
		k.initialized.Store(false)
		return fmt.Errorf("initializing settings: %w", err)
	}
	return nil
}

func initialize(dataDir string) error {
	k.k = koanf.New(".")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %v", err)
	}
	filePath := filepath.Join(dataDir, settingsFileName)
	// 1. Try to atomically read the existing config file
	if raw, err := atomicfile.ReadFile(filePath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// 2. If it exists but is invalid, return an error
			return fmt.Errorf("error loading koanf config file: %w", err)
		} else {
			// 3. If it doesn't exist, create it with default settings
			if err := setDefaults(filePath); err != nil {
				return fmt.Errorf("error setting defaults %w", err)
			}
		}
	} else {
		// 4. If it exists and is valid, load it into koanf
		if err := k.k.Load(rawbytes.Provider(raw), k.parser); err != nil {
			return fmt.Errorf("error parsing koanf config file: %w", err)
		}
	}
	Set(DataPathKey, dataDir)
	return nil
}

func setDefaults(filePath string) error {
	// We need to set the file path first, as otherwise the save function can't read it to save!
	if err := Set(filePathKey, filePath); err != nil {
		return fmt.Errorf("failed to set file path: %w", err)
	}
	if err := Set(LocaleKey, "fa-IR"); err != nil {
		return fmt.Errorf("failed to set default locale: %w", err)
	}
	if err := Set(UserLevelKey, "free"); err != nil {
		return fmt.Errorf("failed to set default user level: %w", err)
	}
	return nil
}

// InitReadOnly initializes the settings in read-only mode from the given directory. InitReadOnly
// does not create a file if it does not exist and instead returns an error. In read-only mode, no
// changes to settings can be made. If watchFile is true, changes to the file on disk will be
// reloaded automatically.
func InitReadOnly(fileDir string, watchFile bool) (err error) {
	if k.initialized.Swap(true) {
		return nil
	}
	defer func() {
		if err != nil {
			k.initialized.Store(false)
		}
	}()
	k.readOnly.Store(true)
	path := filepath.Join(fileDir, settingsFileName)
	if err := reloadSettings(path); err != nil {
		return fmt.Errorf("initializing read-only settings: %w", err)
	}
	if watchFile {
		watcher := internal.NewFileWatcher(path, func() {
			if err := reloadSettings(path); err != nil {
				slog.Error("reloading settings file", "error", err)
			}
		})
		if err := watcher.Start(); err != nil {
			return fmt.Errorf("starting settings file watcher: %w", err)
		}
		k.watcher = watcher
	}
	return nil
}

func reloadSettings(path string) error {
	contents, err := atomicfile.ReadFile(path)
	if err != nil { // including os.ErrNotExist as we only want read-only here
		return fmt.Errorf("loading settings (read-only): %w", err)
	}
	kk := koanf.New(".")
	if err := kk.Load(rawbytes.Provider(contents), k.parser); err != nil {
		return fmt.Errorf("parsing settings: %w", err)
	}
	k.k = kk
	return nil
}

// StopWatching stops watching the settings file for changes. This is only relevant in read-only mode.
func StopWatching() {
	if k.initialized.Load() && k.watcher != nil {
		k.watcher.Close()
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
	out, err := k.k.Marshal(k.parser)
	if err != nil {
		return fmt.Errorf("could not marshal koanf file: %w", err)
	}

	err = atomicfile.WriteFile(GetString(filePathKey), out, 0644)
	if err != nil {
		return fmt.Errorf("could not write koanf file: %w", err)
	}
	return nil
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
