/*
Package transport provides a way to create [transport.StreamDialer] from a [config.Config].

A [transport.StreamDialer] is used to dial a target server using a specific protocol. _i.e._, Shadowsocks, SOCKS5, etc. Before a [transport.StreamDialer] can be created,
a [BuilderFn] must be registered for the desired protocol, see [README]. The builder function is responsible for creating the [transport.StreamDialer]
using the provided configuration.
*/
package transport

import (
	"fmt"

	"github.com/Jigsaw-Code/outline-sdk/transport"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
)

// BuilderFn is a function that creates a new StreamDialer wrapping innerSD.
type BuilderFn func(innerSD transport.StreamDialer, conf *config.Config) (transport.StreamDialer, error)

var (
	// dialerBuilders is a map of builder functions for each supported protocol.
	dialerBuilders = make(map[string]BuilderFn)

	log = golog.LoggerFor("transport")
)

// DialerFrom creates a new StreamDialer from [config.Config].
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
	dialer, _ = dialerBuilders["logger"](dialer, config)
	return dialer, nil
}

// registerDialerBuilder registers a builder function for the specified protocol.
func registerDialerBuilder(protocol string, builder BuilderFn) error {
	if _, ok := dialerBuilders[protocol]; ok {
		return fmt.Errorf("Builder already registered for protocol: %v", protocol)
	}

	log.Debugf("Registering builder for protocol: %v", protocol)
	dialerBuilders[protocol] = builder
	return nil
}

// SupportedDialers returns a list of registered dialers that are available for use.
func SupportedDialers() []string {
	var availableDialers []string
	for dialer := range dialerBuilders {
		availableDialers = append(availableDialers, dialer)
	}
	return availableDialers
}
