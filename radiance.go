package radiance

import (
	"fmt"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/proxy"
)

var log = golog.LoggerFor("radiance")

type Radiance struct {
	proxy *proxy.Proxy
}

func (r *Radiance) Run(addr string) error {
	proxyConfig, err := config.GetConfig()
	if err != nil {
		return err
	}
	proxy, err := proxy.NewProxy(proxyConfig)
	if err != nil {
		return fmt.Errorf("Could not create proxy: %w", err)
	}
	r.proxy = proxy

	return proxy.ListenAndServe(addr)
}
