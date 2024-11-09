package transport

import (
	"fmt"

	"github.com/Jigsaw-Code/outline-sdk/transport"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
)

// BuilderFn is a function that creates a new StreamDialer.
type BuilderFn func(innerSD transport.StreamDialer, conf *config.Config) (transport.StreamDialer, error)

var (
	// dialerBuilders is a map of builder functions for each supported protocol.
	dialerBuilders = make(map[string]BuilderFn)

	log = golog.LoggerFor("transport.dialer")
)

// DialerFrom creates a new StreamDialer from the provided configuration.
func DialerFrom(config *config.Config) (transport.StreamDialer, error) {
	builder, ok := dialerBuilders[config.Protocol]
	if !ok {
		return nil, fmt.Errorf("Unsupported protocol: %v", config.Protocol)
	}

	dialer, err := builder(&transport.TCPDialer{}, config)
	if err != nil {
		return nil, fmt.Errorf("Failed to create %s dialer: %w", config.Protocol, err)
	}

	dialer, _ = dialerBuilders["multiplex"](dialer, config)
	// dialer, _ = logger.NewStreamDialer(dialer, config)
	return dialer, nil
}

// RegisterDialerBuilder registers a builder function for the specified protocol.
func RegisterDialerBuilder(protocol string, builder BuilderFn) error {
	if _, ok := dialerBuilders[protocol]; ok {
		return fmt.Errorf("Builder already registered for protocol: %v", protocol)
	}

	log.Debugf("Registering builder for protocol: %v", protocol)
	dialerBuilders[protocol] = builder
	return nil
}
