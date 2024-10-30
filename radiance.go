package radiance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/proxy"
)

var log = golog.LoggerFor("radiance")

type Radiance struct {
	proxy *proxy.Proxy
}

func New() (*Radiance, error) {
	return &Radiance{}, nil
}

func (r *Radiance) Run(addr string) error {
	proxyConfig := config.GetConfig()
	proxy, err := proxy.NewProxy(proxyConfig)
	if err != nil {
		return fmt.Errorf("Could not create proxy: %w", err)
	}
	r.proxy = proxy

	go func() {
		if err := proxy.ListenAndServe(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("Proxy failed: %v", err)
		}
	}()

	return nil
}

func (r *Radiance) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.proxy.Shutdown(ctx); err != nil {
		return fmt.Errorf("Failed to shutdown proxy gracefully: %w", err)
	}
	return nil
}
