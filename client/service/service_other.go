//go:build !gomobile

package boxservice

import (
	"github.com/sagernet/sing-box/experimental"
	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"
)

// newLibbox creates a new libbox.BoxService instance. The platform interface is not used.
func newLibbox(_ libbox.PlatformInterface) (*libbox.BoxService, error) {
	experimental.RegisterClashServerConstructor(clashapi.NewServer)
	return new(libbox.BoxService), nil
}

func (bs *BoxService) Start() error {
	return bs.instance.Start()
}

func (bs *BoxService) Close() error {
	return bs.instance.Close()
}
