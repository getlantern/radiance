package radiance

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/radiance/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
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

func TestNewRadiance(t *testing.T) {
	t.Run("it should return a new Radiance instance", func(t *testing.T) {
		r := NewRadiance()
		assert.NotNil(t, r)
		assert.NotNil(t, r.confHandler)
		assert.Nil(t, r.srv)
		assert.False(t, r.connected)
		assert.NotEmpty(t, r.tunStatus)
		assert.NotNil(t, r.statusMutex)
		assert.NotNil(t, r.stopChan)
	})
}

func TestStartVPN(t *testing.T) {
	var tests = []struct {
		name      string
		setup     func(*gomock.Controller) *Radiance
		givenAddr string
		assert    func(*testing.T, *Radiance, error)
	}{
		{
			name: "it should return an error when failed to get config",
			setup: func(ctrl *gomock.Controller) *Radiance {
				configHandler := NewMockconfigHandler(ctrl)
				r := &Radiance{
					confHandler: configHandler,
					statusMutex: new(sync.Mutex),
					tunStatus:   DisconnectedTUNStatus,
				}
				configHandler.EXPECT().GetConfig(gomock.Any()).Return(nil, assert.AnError)

				return r
			},
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.Error(t, err)
				assert.False(t, r.connectionStatus())
				assert.Equal(t, DisconnectedTUNStatus, r.VPNStatus())
			},
		},
		{
			name: "it should return an error when providing an invalid config",
			setup: func(ctrl *gomock.Controller) *Radiance {
				configHandler := NewMockconfigHandler(ctrl)
				r := &Radiance{
					confHandler: configHandler,
					statusMutex: new(sync.Mutex),
					tunStatus:   DisconnectedTUNStatus,
				}
				configHandler.EXPECT().GetConfig(gomock.Any()).Return(&config.Config{}, nil)

				return r
			},
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.Error(t, err)
				assert.False(t, r.connectionStatus())
				assert.Equal(t, DisconnectedTUNStatus, r.VPNStatus())
			},
		},
		{
			name: "it should succeed when providing valid config and address",
			setup: func(ctrl *gomock.Controller) *Radiance {
				configHandler := NewMockconfigHandler(ctrl)
				r := &Radiance{
					confHandler: configHandler,
					statusMutex: new(sync.Mutex),
					tunStatus:   DisconnectedTUNStatus,
				}
				configHandler.EXPECT().GetConfig(gomock.Any()).Return(&config.Config{Protocol: "logger"}, nil)

				return r
			},
			givenAddr: "127.0.0.1:6666",
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.NoError(t, err)
				assert.True(t, r.connectionStatus())
				// TODO: update assert when using TUN
				assert.Equal(t, DisconnectedTUNStatus, r.VPNStatus())
				require.NoError(t, r.srv.Shutdown(context.Background()))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			r := tt.setup(ctrl)
			errChan := make(chan error)
			go func() {
				errChan <- r.Run(tt.givenAddr)
				close(errChan)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			select {
			case err := <-errChan:
				tt.assert(t, r, err)
			case <-ctx.Done():
				// the server probably started listening successfully and we can assert and stop running
				tt.assert(t, r, nil)
			}
		})
	}
}

func TestStopVPN(t *testing.T) {
	var tests = []struct {
		name   string
		setup  func(*gomock.Controller) *Radiance
		assert func(*testing.T, *Radiance, error)
	}{
		{
			name: "it should return nil when VPN is disconnected",
			setup: func(ctrl *gomock.Controller) *Radiance {
				return &Radiance{
					statusMutex: new(sync.Mutex),
				}
			},
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.NoError(t, err)
			},
		},
		{
			name: "it should return an error when http server is nil",
			setup: func(ctrl *gomock.Controller) *Radiance {
				return &Radiance{
					statusMutex: new(sync.Mutex),
					connected:   true,
				}
			},
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.Error(t, err)
			},
		},
		{
			name: "it should return an error when failed to shutdown http server",
			setup: func(ctrl *gomock.Controller) *Radiance {
				server := NewMockhttpServer(ctrl)
				r := &Radiance{
					srv:         server,
					statusMutex: new(sync.Mutex),
					connected:   true,
				}
				server.EXPECT().Shutdown(gomock.Any()).Return(assert.AnError)
				return r
			},
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.Error(t, err)
			},
		},
		{
			name: "it should succeed when stopping radiance",
			setup: func(ctrl *gomock.Controller) *Radiance {
				server := NewMockhttpServer(ctrl)
				configHandler := NewMockconfigHandler(ctrl)
				r := &Radiance{
					srv:         server,
					confHandler: configHandler,
					statusMutex: new(sync.Mutex),
					connected:   true,
					stopChan:    make(chan struct{}),
				}
				server.EXPECT().Shutdown(gomock.Any()).Return(nil).Times(1)
				configHandler.EXPECT().Stop().Times(1)
				go func() {
					_, ok := <-r.stopChan
					assert.False(t, ok, "stopChan should be closed")
				}()
				return r
			},
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.NoError(t, err)
				assert.False(t, r.connectionStatus())
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			ctx := context.Background()
			r := tt.setup(ctrl)
			err := r.Shutdown(ctx)
			tt.assert(t, r, err)
		})
	}
}

func TestVPNStatus(t *testing.T) {
	r := &Radiance{
		statusMutex: new(sync.Mutex),
		tunStatus:   DisconnectedTUNStatus,
	}
	assert.Equal(t, DisconnectedTUNStatus, r.VPNStatus())
	r.setStatus(false, ConnectedTUNStatus)
	assert.Equal(t, ConnectedTUNStatus, r.VPNStatus())

	r.setStatus(false, ConnectingTUNStatus)
	assert.Equal(t, ConnectingTUNStatus, r.VPNStatus())
}

func TestActiveProxyLocation(t *testing.T) {
	expectedCity := "New York"
	var tests = []struct {
		name   string
		setup  func(*gomock.Controller) *Radiance
		assert func(*testing.T, string)
	}{
		{
			name: "it should return nil when VPN is disconnected and return an error",
			setup: func(ctrl *gomock.Controller) *Radiance {
				r := &Radiance{
					statusMutex: new(sync.Mutex),
				}
				return r
			},
			assert: func(t *testing.T, location string) {
				assert.Empty(t, location)
			},
		},
		{
			name: "it should return nil when failed to retrieve config",
			setup: func(ctrl *gomock.Controller) *Radiance {
				configHandler := NewMockconfigHandler(ctrl)
				r := &Radiance{
					confHandler: configHandler,
					statusMutex: new(sync.Mutex),
					connected:   true,
				}
				configHandler.EXPECT().GetConfig(gomock.Any()).Return(nil, assert.AnError)
				return r
			},
			assert: func(t *testing.T, location string) {
				assert.Empty(t, location)
			},
		},
		{
			name: "it should return the location when VPN is connected",
			setup: func(ctrl *gomock.Controller) *Radiance {
				configHandler := NewMockconfigHandler(ctrl)
				r := &Radiance{
					confHandler: configHandler,
					statusMutex: new(sync.Mutex),
					connected:   true,
				}
				config := config.Config{
					Location: &config.ProxyConnectConfig_ProxyLocation{City: expectedCity},
				}
				configHandler.EXPECT().GetConfig(gomock.Any()).Return(&config, nil)
				return r
			},
			assert: func(t *testing.T, location string) {
				assert.NotEmpty(t, location)
				assert.Equal(t, expectedCity, location)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			r := tt.setup(ctrl)
			location := r.ActiveProxyLocation(context.Background())
			tt.assert(t, location)
		})
	}
}

func TestProxyStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	expectedCity := "New York"
	configHandler := NewMockconfigHandler(ctrl)
	config := config.Config{
		Location: &config.ProxyConnectConfig_ProxyLocation{City: expectedCity},
	}
	configHandler.EXPECT().GetConfig(gomock.Any()).Return(&config, nil)

	r := &Radiance{
		statusMutex: new(sync.Mutex),
		connected:   false,
		stopChan:    make(chan struct{}),
		confHandler: configHandler,
	}
	pollInterval := 1 * time.Millisecond
	var statusChan <-chan ProxyStatus
	t.Run("it should return a not connected status", func(t *testing.T) {
		statusChan = r.ProxyStatus(pollInterval)
		assert.NotNil(t, statusChan)
		status, ok := <-statusChan
		assert.True(t, ok)
		assert.False(t, status.Connected)
		assert.Empty(t, status.Location)
	})

	t.Run("it should return the proxy status with location when proxy is connected", func(t *testing.T) {
		r.setStatus(true, ConnectedTUNStatus)
		status, ok := <-statusChan
		assert.True(t, ok)
		assert.True(t, status.Connected)
		assert.Equal(t, expectedCity, status.Location)
	})

	t.Run("it should return the proxy status when VPN is disconnected", func(t *testing.T) {
		r.setStatus(false, DisconnectedTUNStatus)
		status, ok := <-statusChan
		assert.True(t, ok)
		assert.False(t, status.Connected)
		assert.Empty(t, status.Location)
	})

	t.Run("it should close the channel when stopChan is closed", func(t *testing.T) {
		close(r.stopChan)
		_, ok := <-statusChan
		assert.False(t, ok)

		secondStatusChan := r.ProxyStatus(pollInterval)
		_, ok = <-secondStatusChan
		assert.False(t, ok)
	})
}
