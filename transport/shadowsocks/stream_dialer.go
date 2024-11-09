package shadowsocks

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	mrand "math/rand"
	"sync"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/transport/shadowsocks"

	"github.com/getlantern/errors"

	"github.com/getlantern/radiance/config"
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
	rng      *mrand.Rand
	rngmx    sync.Mutex
}

// NewStreamDialer creates a new shadowsocks StreamDialer using the provided configuration.
// The returned StreamDialer will route traffic through the provided inner StreamDialer.
func NewStreamDialer(innerSD transport.StreamDialer, cfg *config.Config) (transport.StreamDialer, error) {
	ssconf := cfg.GetConnectCfgShadowsocks()
	if ssconf == nil {
		return nil, errors.New("config is not a shadowsocks config")
	}
	key, err := shadowsocks.NewEncryptionKey(ssconf.Cipher, ssconf.Secret)
	if err != nil {
		return nil, errors.New("failed to create shadowsocks key: %v", err)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)
	endpoint := &transport.StreamDialerEndpoint{Dialer: innerSD, Address: addr}
	dialer, err := shadowsocks.NewStreamDialer(endpoint, key)
	if err != nil {
		return nil, errors.New("failed to create shadowsocks client: %v", err)
	}

	// Infrastructure python code seems to insert "None" as the prefix generator if there is none.
	prefixGen := ssconf.PrefixGenerator
	if prefixGen != "" && prefixGen != "None" {
		dialer.SaltGenerator = shadowsocks.NewPrefixSaltGenerator([]byte(prefixGen))
	}

	var seed int64
	err = binary.Read(crand.Reader, binary.BigEndian, &seed)
	if err != nil {
		return nil, errors.New("unable to initialize rng: %v", err)
	}
	source := mrand.NewSource(seed)
	rng := mrand.New(source)

	return &StreamDialer{dialer, defaultShadowsocksUpstreamSuffix, rng, sync.Mutex{}}, nil
}

// DialStream implements the transport.StreamDialer interface.
func (sd *StreamDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	return sd.innerSD.DialStream(ctx, sd.generateUpstream())
}

// generateUpstream() creates a marker upstream address.  This isn't an
// acutal upstream that will be dialed, it signals that the upstream
// should be determined by other methods.  It's just a bit random just to
// mix it up and not do anything especially consistent on every dial.
//
// To satisy shadowsocks expectations, a small random string is prefixed onto the
// configured suffix (along with a .) and a port is affixed to the end.
func (sd *StreamDialer) generateUpstream() string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	sd.rngmx.Lock()
	defer sd.rngmx.Unlock()
	// [2 - 22]
	sz := 2 + sd.rng.Intn(21)
	b := make([]byte, sz)
	for i := range b {
		b[i] = letters[sd.rng.Intn(len(letters))]
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
