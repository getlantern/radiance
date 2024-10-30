package config

var config string

func GetConfig() string {
	return config
}

// TEMP: SetConfig is a temporary function to assign the config.
//
// SetConfig is for testing purposes only. Once the ability to load/retreive a real proxy config is
// implemented, then it will be removed. Do not use for any other purpose.
func SetConfig(conf string) {
	config = conf
}
