package rvpn

import "github.com/sagernet/sing-box/experimental/libbox"

type PlatformInterface interface {
	libbox.PlatformInterface
	RestartService() error
	PostServiceClose()
}
