package tunnel

import (
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	lwip "github.com/eycorsican/go-tun2socks/core"
	"github.com/eycorsican/go-tun2socks/proxy/dnsfallback"
	"github.com/getlantern/golog"
)

var (
	log        = golog.LoggerFor("vpn.tunnel")
	udpTimeout = 30 * time.Second
)

type Tunnel struct {
	lwip.LWIPStack
	sd         transport.StreamDialer
	pd         transport.PacketListener
	udpEnabled bool
	isClosed   atomic.Bool
}

func NewTunnel(sd transport.StreamDialer, pd transport.PacketListener, udpEnabled bool, tunWriter io.WriteCloser) (*Tunnel, error) {
	if tunWriter == nil {
		return nil, errors.New("tunWriter is required")
	}
	lwipStack := lwip.NewLWIPStack()
	if udpEnabled {
		lwip.RegisterUDPConnHandler(newUDPHandler(pd, udpTimeout))
	} else {
		lwip.RegisterUDPConnHandler(dnsfallback.NewUDPHandler())
	}
	lwip.RegisterTCPConnHandler(newTCPHandler(sd))
	lwip.RegisterOutputFn(func(data []byte) (int, error) {
		log.Tracef("proxy outputFn writing %d bytes to tunDevice", len(data))
		return tunWriter.Write(data)
	})

	log.Debug("tunnel created")
	return &Tunnel{
		LWIPStack:  lwipStack,
		sd:         sd,
		pd:         pd,
		udpEnabled: udpEnabled,
		isClosed:   atomic.Bool{},
	}, nil
}

func (t *Tunnel) Write(data []byte) (int, error) {
	if t.isClosed.Load() {
		log.Debug("tunnel is closed")
		return 0, errors.New("tunnel is closed")
	}
	log.Tracef("writing %d bytes to tunnel", len(data))
	return t.LWIPStack.Write(data)
}

func (t *Tunnel) Close() error {
	if t.isClosed.CompareAndSwap(false, true) {
		log.Debug("closing tunnel")
		return t.LWIPStack.Close()
	}
	return nil
}
