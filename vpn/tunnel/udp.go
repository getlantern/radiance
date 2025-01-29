package tunnel

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	lwip "github.com/eycorsican/go-tun2socks/core"
)

// udpHandler handles UDP connections.
// based on https://github.com/Jigsaw-Code/outline-go-tun2socks/blob/master/outline/tun2socks/udp.go
type udpHandler struct {
	// Used to establish connections to the proxy
	listener transport.PacketListener
	timeout  time.Duration
	// Maps connections from TUN to connections to the proxy.
	conns map[lwip.UDPConn]net.PacketConn
	// Protects the connections map
	sync.Mutex
}

// newUDPHandler returns a new UDP connection handler. pktListener is used to establish connections
// to the proxy. `timeout` is the UDP read and write timeout.
func newUDPHandler(pktListener transport.PacketListener, timeout time.Duration) lwip.UDPConnHandler {
	return &udpHandler{
		listener: pktListener,
		timeout:  timeout,
		conns:    make(map[lwip.UDPConn]net.PacketConn, 8),
	}
}

// Connect establishes a connection to the proxy and relays packets between the TUN device and the
// proxy.
func (h *udpHandler) Connect(tunConn lwip.UDPConn, target *net.UDPAddr) error {
	log.Debugf("Handling UDP connection to %s", target.String())
	proxyConn, err := h.listener.ListenPacket(context.Background())
	if err != nil {
		return err
	}
	h.Lock()
	h.conns[tunConn] = proxyConn
	h.Unlock()
	go h.relayPacketsFromProxy(tunConn, proxyConn)
	return nil
}

// relayPacketsFromProxy relays packets from the proxy to the TUN device.
func (h *udpHandler) relayPacketsFromProxy(tunConn lwip.UDPConn, proxyConn net.PacketConn) {
	buf := lwip.NewBytes(lwip.BufSize)
	defer func() {
		h.close(tunConn)
		lwip.FreeBytes(buf)
	}()
	for {
		proxyConn.SetDeadline(time.Now().Add(h.timeout))
		n, sourceAddr, err := proxyConn.ReadFrom(buf)
		if err != nil {
			log.Errorf("failed to read UDP data from proxy: %v", err)
			return
		}
		// No resolution will take place, the address sent by the proxy is a resolved IP.
		sourceUDPAddr, err := net.ResolveUDPAddr("udp", sourceAddr.String())
		if err != nil {
			return
		}
		_, err = tunConn.WriteFrom(buf[:n], sourceUDPAddr)
		if err != nil {
			log.Errorf("failed to write UDP date to TUN device: %v", err)
			return
		}
	}
}

// ReceiveTo relays packets from the TUN device to the proxy. It's called by tun2socks.
func (h *udpHandler) ReceiveTo(tunConn lwip.UDPConn, data []byte, destAddr *net.UDPAddr) error {
	h.Lock()
	proxyConn, ok := h.conns[tunConn]
	h.Unlock()
	if !ok {
		return fmt.Errorf("connection %v->%v does not exist", tunConn.LocalAddr(), destAddr)
	}
	proxyConn.SetDeadline(time.Now().Add(h.timeout))
	_, err := proxyConn.WriteTo(data, destAddr)
	return err
}

func (h *udpHandler) close(tunConn lwip.UDPConn) {
	tunConn.Close()
	h.Lock()
	defer h.Unlock()
	if proxyConn, ok := h.conns[tunConn]; ok {
		proxyConn.Close()
	}
}
