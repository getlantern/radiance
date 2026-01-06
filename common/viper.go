package common

import (
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
	viper.SetDefault(LocaleKey, "fa-IR")
	return viper.ReadInConfig()
}
