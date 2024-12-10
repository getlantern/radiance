/*
Package shadowsocks provides a [transport.StreamDialer] that routes traffic through a shadowsocks proxy.

Config values:

	"cipher": Cipher to use to create the encryption key. REQUIRED
	"secret": The secret key to use to create the encryption key. REQUIRED
	"upstream": This is used to signal that the upstream should be determined by other methods and to add some randomness.
	"prefixgenerator": Generator function to create prefixes.
*/
package shadowsocks

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"math/rand/v2"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/transport/shadowsocks"

	"github.com/getlantern/errors"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport/shadowsocks/prefixgen"
)

const (
	defaultShadowsocksUpstreamSuffix = "test"
)

// StreamDialer routes traffic through a shadowsocks proxy.
type StreamDialer struct {
	// innerSD is the shadowsocks stream dialer that has been configured with the key, endpoint, etc.
	innerSD *shadowsocks.StreamDialer
	// upstream is the suffix of the upstream address to dial.
	upstream string
}

// NewStreamDialer creates a new shadowsocks StreamDialer using the provided configuration.
// The returned StreamDialer will route traffic through the provided inner StreamDialer.
func NewStreamDialer(innerSD transport.StreamDialer, config config.Config) (transport.StreamDialer, error) {
	ssconf := config.Shadowsocks
	key, err := shadowsocks.NewEncryptionKey(ssconf["cipher"], ssconf["secret"])
	if err != nil {
		return nil, errors.New("failed to create shadowsocks key: %v", err)
	}

	addr := fmt.Sprintf("%s:%d", config.Addr, config.Port)
	endpoint := &transport.StreamDialerEndpoint{Dialer: innerSD, Address: addr}
	dialer, err := shadowsocks.NewStreamDialer(endpoint, key)
	if err != nil {
		return nil, errors.New("failed to create shadowsocks client: %v", err)
	}

	prefixGen := ssconf["prefixgenerator"]
	if prefixGen != "" {
		gen, err := prefixgen.New(prefixGen)
		if err != nil {
			return nil, errors.New("failed to create prefix generator: %v", err)
		}
		dialer.SaltGenerator = &PrefixSaltGen{
			prefixFunc: func() ([]byte, error) { return gen(), nil },
		}
	}

	upstream := ssconf["upstream"]
	if upstream == "" {
		upstream = defaultShadowsocksUpstreamSuffix
	}

	return &StreamDialer{dialer, upstream}, nil
}

// DialStream implements the transport.StreamDialer interface.
func (sd *StreamDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	return sd.innerSD.DialStream(ctx, sd.generateUpstream())
}

// generateUpstream() creates a marker upstream address.  This isn't an
// actual upstream that will be dialed, it signals that the upstream
// should be determined by other methods.  It's just a bit random just to
// mix it up and not do anything especially consistent on every dial.
//
// To satisy shadowsocks expectations, a small random string is prefixed onto the
// configured suffix (along with a .) and a port is affixed to the end.
func (sd *StreamDialer) generateUpstream() string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	// [2 - 22]
	sz := 2 + rand.IntN(21)
	b := make([]byte, sz)
	for i := range b {
		b[i] = letters[rand.IntN(len(letters))]
	}

	return string(b) + "." + sd.upstream + ":443"
}

type PrefixSaltGen struct {
	prefixFunc func() ([]byte, error)
}

func (p *PrefixSaltGen) GetSalt(salt []byte) error {
	prefix, err := p.prefixFunc()
	if err != nil {
		return fmt.Errorf("failed to generate prefix: %v", err)
	}
	n := copy(salt, prefix)
	if n != len(prefix) {
		return errors.New("prefix is too long")
	}
	_, err = crand.Read(salt[n:])
	return err
}
