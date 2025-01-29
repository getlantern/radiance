package tunnel

import (
	"context"
	"io"
	"net"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/eycorsican/go-tun2socks/core"
)

// tcpHandler handles incoming TCP connections and establishes proxy connections.
// based on https://github.com/Jigsaw-Code/outline-apps/blob/master/client/go/outline/tun2socks/tcp.go
type tcpHandler struct {
	dialer transport.StreamDialer
}

// newTCPHandler returns a TCP connection handler.
func newTCPHandler(sd transport.StreamDialer) *tcpHandler {
	return &tcpHandler{sd}
}

// Handle establishes a connection to the proxy and relays packets between the client and the proxy.
func (h *tcpHandler) Handle(conn net.Conn, target *net.TCPAddr) error {
	log.Debugf("Handling TCP connection to %s", target.String())
	proxyConn, err := h.dialer.DialStream(context.Background(), target.String())
	if err != nil {
		log.Errorf("Failed to dial to %s: %v", target.String(), err)
		return err
	}
	go relay(conn.(core.TCPConn), proxyConn)
	return nil
}

// copyOneWay copies from rightConn to leftConn until either EOF is reached on rightConn or an error occurs.
//
// If rightConn implements io.WriterTo, or if leftConn implements io.ReaderFrom, copyOneWay will leverage these
// interfaces to do the copy as a performance improvement method.
//
// rightConn's read end and leftConn's write end will be closed after copyOneWay returns.
func copyOneWay(leftConn, rightConn transport.StreamConn) (int64, error) {
	n, err := io.Copy(leftConn, rightConn)
	// Send FIN to indicate EOF
	leftConn.CloseWrite()
	// Release reader resources
	rightConn.CloseRead()
	return n, err
}

// relay copies between left and right bidirectionally. Returns number of
// bytes copied from right to left, from left to right, and any error occurred.
// Relay allows for half-closed connections: if one side is done writing, it can
// still read all remaining data from its peer.
func relay(leftConn, rightConn transport.StreamConn) (int64, int64, error) {
	log.Debug("Relaying TCP connection")
	type res struct {
		N   int64
		Err error
	}
	ch := make(chan res)

	go func() {
		n, err := copyOneWay(rightConn, leftConn)
		ch <- res{n, err}
	}()

	n, err := copyOneWay(leftConn, rightConn)
	rs := <-ch

	if err == nil {
		err = rs.Err
	}
	return n, rs.N, err
}
