package common

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/viper"
)

// LocaleKey is the key used to store and retrieve the user's locale setting, which is typically
// passed in from the frontend and used to customize user experience based on their language.
const LocaleKey = "locale"

// Initializes the viper config for user settings, which can be used by both the tunnel process and
// the main application process to read user preferences like locale.
func initUserConfig(dataDir string) error {
	viper.SetConfigName("local")
	viper.AddConfigPath(dataDir)
	viper.SetConfigType("json")
	var fileLookupError viper.ConfigFileNotFoundError
	if err := viper.ReadInConfig(); err != nil {
		if errors.As(err, &fileLookupError) {
			// Indicates an explicitly set config file is not found (such as with
			// using `viper.SetConfigFile`) or that no config file was found in
			// any search path (such as when using `viper.AddConfigPath`)
			// This likely means first run, so we can ignore this error.
			slog.Info("No existing user config file found; assuming first run", "path", dataDir)
		} else {
			// Config file was found but another error was produced.
			return fmt.Errorf("Error loading config file %w", err)
		}
	}

	// Config file found and successfully parsed

	viper.SetDefault(LocaleKey, "fa-IR")
	viper.WriteConfig()
	return nil
}
