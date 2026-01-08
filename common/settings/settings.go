package settings

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// LocaleKey is the key used to store and retrieve the user's locale setting, which is typically
// passed in from the frontend and used to customize user experience based on their language.
const (
	CountryCodeKey = "country_code"
	LocaleKey      = "locale"
)

// InitSettings initializes the viper config for user settings, which can be used by both the tunnel process and
// the main application process to read user preferences like locale.
func InitSettings(dataDir string) error {
	viper.SetConfigName("local.json")
	viper.AddConfigPath(dataDir)
	viper.SetConfigType("json")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %v", err)
	}
	var fileLookupError viper.ConfigFileNotFoundError
	if err := viper.ReadInConfig(); err != nil {
		if errors.As(err, &fileLookupError) {
			// Indicates an explicitly set config file is not found (such as with
			// using `viper.SetConfigFile`) or that no config file was found in
			// any search path (such as when using `viper.AddConfigPath`)
			// This likely means first run, so we can ignore this error.
			slog.Info("No existing user config file found; assuming first run", "path", dataDir)
			viper.SetDefault(LocaleKey, "fa-IR")

			if writeErr := viper.SafeWriteConfigAs(filepath.Join(dataDir, "local.json")); writeErr != nil {
				slog.Error("Failed to write default config file", "path", dataDir, "error", writeErr)
				return fmt.Errorf("Error writing default config file %w", writeErr)
			}
		} else {
			// Config file was found but another error was produced.
			return fmt.Errorf("Error loading config file %w", err)
		}
	}
	// Config file found and successfully parsed
	return nil
}
