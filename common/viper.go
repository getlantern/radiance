package common

import (
	"github.com/spf13/viper"
)

const LocaleKey = "locale"

func initUserConfig(dataDir string) error {
	viper.SetConfigName("local")
	viper.AddConfigPath(dataDir)
	viper.SetConfigType("json")
	viper.SetDefault(LocaleKey, "fa-IR")
	return viper.ReadInConfig()
}
