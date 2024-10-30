package shadowsocks

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	goerrors "errors"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"sync"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/transport/shadowsocks"

	"github.com/getlantern/errors"
)

const (
	defaultShadowsocksUpstreamSuffix = "test"
)

type StreamDialer struct {
	*shadowsocks.StreamDialer
	upstream string
	rng      *mrand.Rand
	rngmx    sync.Mutex
}

func WrapStreamDialer(innerSD transport.StreamDialer, config map[string]string) (transport.StreamDialer, error) {
	secret := config["shadowsocks_secret"]
	cipher := config["shadowsocks_cipher"]
	key, err := shadowsocks.NewEncryptionKey(cipher, secret)
	if err != nil {
		return nil, errors.New("failed to create shadowsocks key: %v", err)
	}

	addr := config["addr"]
	endpoint := &transport.StreamDialerEndpoint{Dialer: innerSD, Address: addr}
	dialer, err := shadowsocks.NewStreamDialer(endpoint, key)
	if err != nil {
		return nil, errors.New("failed to create shadowsocks client: %v", err)
	}

	// Infrastructure python code seems to insert "None" as the prefix generator if there is none.
	prefixGen := config["shadowsocks_prefix_generator"]
	if prefixGen != "" && prefixGen != "None" {
		dialer.SaltGenerator = shadowsocks.NewPrefixSaltGenerator([]byte(prefixGen))
		// gen, err := prefixgen.New(prefixGen)
		// name, _ := config["name"]
		// if err != nil {
		// 	log.Errorf("failed to parse shadowsocks prefix generator from %v for proxy %v: %v", prefixGen, name, err)
		// 	return nil, errors.New("failed to parse shadowsocks prefix generator from %v for proxy %v: %v", prefixGen, name, err)
		// }
		// prefixFunc := func() ([]byte, error) { return gen(), nil }
		// dialer.SaltGenerator = &PrefixSaltGen{prefixFunc}
	}

	var seed int64
	err = binary.Read(crand.Reader, binary.BigEndian, &seed)
	if err != nil {
		return nil, errors.New("unable to initialize rng: %v", err)
	}
	source := mrand.NewSource(seed)
	rng := mrand.New(source)

	// TODO: if tls pass as innerSD
	// withTLSStr, _ := config["shadowsocks_with_tls"]
	// withTLS, err := strconv.ParseBool(withTLSStr)
	// if err != nil {
	// 	withTLS = false
	// }
	//
	// var tlsConfig *tls.Config = nil
	// if withTLS {
	// 	certPool := x509.NewCertPool()
	// 	if ok := certPool.AppendCertsFromPEM([]byte(pc.Cert)); !ok {
	// 		return nil, errors.New("couldn't add certificate to pool")
	// 	}
	// 	ip, _, err := net.SplitHostPort(addr)
	// 	if err != nil {
	// 		return nil, errors.New("couldn't split host and port: %v", err)
	// 	}
	//
	// 	tlsConfig = &tls.Config{
	// 		RootCAs:    certPool,
	// 		ServerName: ip,
	// 	}
	// }

	upstream, ok := config["shadowsocks_upstream"]
	if !ok || upstream == "" {
		upstream = defaultShadowsocksUpstreamSuffix
	}

	return &StreamDialer{dialer, upstream, rng, sync.Mutex{}}, nil
}

func (sd *StreamDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	return sd.StreamDialer.DialStream(ctx, sd.generateUpstream())
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

// func (sd *shadowsocksImpl) dialServer(op *ops.Op, ctx context.Context) (net.Conn, error) {
// 	return sd.reportDialCore(op, func() (net.Conn, error) {
// 		conn, err := sd.client.DialStream(ctx, sd.generateUpstream())
// 		if err != nil {
// 			return nil, err
// 		}
// 		if sd.tlsConfig != nil {
// 			tlsConn := tls.Client(conn, sd.tlsConfig)
// 			return &ssWrapConn{tlsConn}, nil
// 		}
// 		return &ssWrapConn{conn}, nil
// 	})
// }

// this is a helper to smooth out error bumps
// that the rest of lantern doesn't really expect, but happen
// in the shadowsocks sd when closing.
type ssWrapConn struct {
	net.Conn
}

func (c *ssWrapConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	return n, ssTranslateError(err)
}

func (c *ssWrapConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	return n, ssTranslateError(err)
}

func ssTranslateError(err error) error {
	if err == nil {
		return nil
	}

	if goerrors.Is(err, net.ErrClosed) {
		return io.EOF
	}

	return err
}
