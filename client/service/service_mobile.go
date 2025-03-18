//go:build ios || android

package boxservice

import (
	"github.com/sagernet/sing-box/experimental/libbox"
)

// newLibbox creates a new libbox.BoxService instance using the provided platform interface and
func newLibbox(platIfce libbox.PlatformInterface) (*libbox.BoxService, error) {
	// throwaway config string in order to create a libbox.BoxService instance as we need the
	// plaforminterfacewrapper
	configStr := "{\"log\":{\"disabled\":true},\"outbounds\":[{\"type\":\"direct\",\"tag\":\"direct\"},{\"type\":\"dns\",\"tag\":\"dns-out\"}],\"route\":{\"rules\":[{\"protocol\":\"dns\",\"outbound\":\"dns-out\"}]}}"
	return libbox.NewService(configStr, platIfce)
}

func (bs *BoxService) Start() error {
	return bs.libbox.Start()
}

func (bs *BoxService) Close() error {
	return bs.libbox.Close()
}
