package settings

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

// LocaleKey is the key used to store and retrieve the user's locale setting, which is typically
// passed in from the frontend and used to customize user experience based on their language.
const (
	CountryCodeKey = "country_code"
	LocaleKey      = "locale"
	DeviceIDKey    = "device_id"
	DataPathKey    = "data_path"
	LoginDataKey   = "login_data"
)

type settings struct {
	k        *koanf.Koanf
	filePath string
	parser   koanf.Parser
}

var k = &settings{
	k:      koanf.New("."),
	parser: json.Parser(),
}

// InitSettings initializes the config for user settings, which can be used by both the tunnel process and
// the main application process to read user preferences like locale.
func InitSettings(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %v", err)
	}
	k.filePath = filepath.Join(dataDir, "local.json")

	// 1. Try to atomically read the existing config file
	if raw, err := atomicfile.ReadFile(k.filePath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// 2. If it exists but is invalid, return an error
			return fmt.Errorf("error loading koanf config file: %w", err)
		} else {
			// 3. If it doesn't exist, create it with default settings
			slog.Info("creating new config file with default settings", "path", k.filePath)
			k.k.Set(LocaleKey, "fa-IR")
			save()
		}
	} else {
		// 4. If it exists and is valid, load it into koanf
		if err := k.k.Load(rawbytes.Provider(raw), k.parser); err != nil {
			return fmt.Errorf("error parsing koanf config file: %w", err)
		}
	}
	return nil
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
	k.k.Set(key, value)
	return save()
}

func save() error {
	out, err := k.k.Marshal(k.parser)
	if err != nil {
		return fmt.Errorf("Could not marshall koanf file: %w", err)
	}

	err = atomicfile.WriteFile(k.filePath, out, 0644)
	if err != nil {
		return fmt.Errorf("Could not write koanf file: %w", err)
	}
	return nil
}
