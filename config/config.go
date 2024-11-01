package config

import (
	_ "embed"
	"encoding/json"
)

//go:embed proxy.conf
var conf []byte

type Config struct {
	Addr           string            `json:"addr"`
	Track          string            `json:"track"`
	Name           string            `json:"name"`
	Port           int               `json:"port"`
	CertPEM        string            `json:"certPem"`
	AuthToken      string            `json:"authToken"`
	ShadowsocksCfg map[string]string `json:"connectCfgShadowsocks"`
}

var config Config

func GetConfig() (Config, error) {
	if err := readConfig(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// TEMP: SetConfig is a temporary function to assign the config.
//
// SetConfig is for testing purposes only. Once the ability to load/retreive a real proxy config is
// implemented, then it will be removed. Do not use for any other purpose.
func SetConfig(conf Config) {
	config = conf
}

func readConfig() error {
	if err := json.Unmarshal(conf, &config); err != nil {
		return err
	}
	return nil
}
