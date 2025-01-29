package proxy

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/stretchr/testify/assert"
)

func TestHandleConnect(t *testing.T) {
	clientConn := newMockStreamConn("client")
	targetConn := newMockStreamConn("target")
	dialer := testDialer(targetConn)
	ph := proxyHandler{
		addr:      "addr",
		authToken: "test",
		dialer:    dialer,
	}
	testReq, _ := http.NewRequest("CONNECT", "https://bk.lounge", nil)
	testResp := &mockResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             clientConn,
	}

	rdy := make(chan struct{}, 1)
	go func() {
		rdy <- struct{}{}
		ph.handleConnect(testResp, testReq)
	}()
	<-rdy

	t.Log("target: reading CONNECT request")
	req, err := http.ReadRequest(bufio.NewReader(targetConn.testConn))
	if assert.NoError(t, err, "failed to read request") {
		assert.Equal(t, "CONNECT", req.Method)
		assert.Equal(t, testReq.URL.Host, req.URL.Host)
		assert.Equal(t, ph.authToken, req.Header.Get(authTokenHeader))
	}
	t.Log("CONNECT successful")

	msgs := []string{
		"Welcome to the bk lounge!",
		"Can we get in?",
		"Not without coups, not without coups baby",
	}
	go func() {
		rdy <- struct{}{}
		buf := make([]byte, 1024)
		n, err := targetConn.testConn.Write([]byte(msgs[0]))
		assert.NoErrorf(t, err, "failed to write to target. wrote %v bytes", n)

		n, err = targetConn.testConn.Read(buf)
		assert.NoErrorf(t, err, "failed to read from target. read %v bytes", n)
		assert.Equal(t, msgs[1], string(buf[:n]))

		n, err = targetConn.testConn.Write([]byte(msgs[2]))
		assert.NoErrorf(t, err, "failed to write to target. wrote %v bytes", n)
	}()
	<-rdy

	buf := make([]byte, 1024)
	n, err := clientConn.testConn.Read(buf)
	assert.NoErrorf(t, err, "failed to read from client. read %v bytes", n)
	assert.Equal(t, msgs[0], string(buf[:n]))

	n, err = clientConn.testConn.Write([]byte(msgs[1]))
	assert.NoErrorf(t, err, "failed to write to client. wrote %v bytes", n)

	n, err = clientConn.testConn.Read(buf)
	assert.NoErrorf(t, err, "failed to read from client. read %v bytes", n)
	assert.Equal(t, msgs[2], string(buf[:n]))

	clientConn.Close()
}

type mockResponseWriter struct {
	*httptest.ResponseRecorder
	conn *mockStreamConn
}

func (mrw *mockResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return mrw.conn, bufio.NewReadWriter(bufio.NewReader(mrw.conn), bufio.NewWriter(mrw.conn)), nil
}

func testDialer(conn *mockStreamConn) transport.StreamDialer {
	return transport.FuncStreamDialer(
		func(ctx context.Context, remoteAddr string) (transport.StreamConn, error) {
			return conn, nil
		},
	)
}

type mockStreamConn struct {
	name string
	transport.StreamConn
	innerConn, testConn net.Conn
}

func newMockStreamConn(name string) *mockStreamConn {
	c1, c2 := net.Pipe()
	return &mockStreamConn{
		name:      name,
		innerConn: c1,
		testConn:  c2,
	}
}

func (msc *mockStreamConn) Close() error { return msc.innerConn.Close() }

func (msc *mockStreamConn) Read(p []byte) (n int, err error) {
	// defer func() { log.Debugf("%v conn: reading\n%s", msc.name, p) }()
	return msc.innerConn.Read(p)
}

func (msc *mockStreamConn) Write(p []byte) (n int, err error) {
	// log.Debugf("%v conn: writing\n%s", msc.name, p)
	return msc.innerConn.Write(p)
}
