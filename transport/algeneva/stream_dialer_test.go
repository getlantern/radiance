package algeneva

import (
	"context"
	"net"
	"testing"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	algeneva "github.com/getlantern/lantern-algeneva"

	"github.com/getlantern/radiance/backend/apipb"
	"github.com/getlantern/radiance/config"
)

func TestNewStreamDialer(t *testing.T) {
	certPem := []byte(`-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----`)
	protocolConfig := &apipb.ProxyConnectConfig_ConnectCfgAlgeneva{
		ConnectCfgAlgeneva: &apipb.ProxyConnectConfig_AlgenevaConfig{Strategy: "test-strategy"},
	}
	tests := []struct {
		name   string
		cfg    *config.Config
		assert func(t *testing.T, got transport.StreamDialer, cfg *config.Config, err error)
	}{
		{
			name: "success",
			cfg: &config.Config{
				Addr:           "1.1.1.1",
				CertPem:        certPem,
				ProtocolConfig: protocolConfig,
			},
			assert: func(t *testing.T, got transport.StreamDialer, cfg *config.Config, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, got)
			},
		},
		{
			name: "missing config",
			assert: func(t *testing.T, got transport.StreamDialer, cfg *config.Config, err error) {
				assert.Error(t, err)
				assert.Nil(t, got)
			},
		},
		{
			name: "invalid cert",
			cfg: &config.Config{
				CertPem:        []byte("alright, you caught me"),
				ProtocolConfig: protocolConfig,
			},
			assert: func(t *testing.T, got transport.StreamDialer, cfg *config.Config, err error) {
				assert.Error(t, err)
				assert.Nil(t, got)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDialer := new(mockStreamDialer)
			got, err := NewStreamDialer(mockDialer, tt.cfg)
			tt.assert(t, got, tt.cfg, err)
		})
	}
}

type mockStreamDialer struct {
	mock.Mock
}

func (m *mockStreamDialer) DialStream(ctx context.Context, remoteAddr string) (transport.StreamConn, error) {
	args := m.Called(ctx, remoteAddr)
	return args.Get(0).(transport.StreamConn), args.Error(1)
}

type mockStreamConn struct {
	mock.Mock
	transport.StreamConn
}

func (m *mockStreamConn) CloseRead() error {
	args := m.Called()
	return args.Error(0)
}

func (m *mockStreamConn) CloseWrite() error {
	args := m.Called()
	return args.Error(0)
}

func TestDialStream(t *testing.T) {
	name := "preserves inner dialer close read/write"
	dl := func(ctx context.Context, network, address string, opts algeneva.DialerOpts) (net.Conn, error) {
		return &net.TCPConn{}, nil
	}
	t.Run(name, func(t *testing.T) {
		mockDialer := new(mockStreamDialer)
		mockConn := new(mockStreamConn)
		ctx := context.Background()

		mockDialer.On("DialStream", ctx, "").Return(mockConn, nil)
		dialer := &StreamDialer{
			innerSD: mockDialer,
			opts:    algeneva.DialerOpts{},
			dialAlg: dl,
		}

		conn, err := dialer.DialStream(ctx, "")
		assert.NoError(t, err)
		assert.NotNil(t, conn)
		mockDialer.AssertExpectations(t)

		mockConn.On("CloseRead").Return(nil)
		conn.CloseRead()
		mockConn.AssertExpectations(t)

		mockConn.On("CloseWrite").Return(nil)
		conn.CloseWrite()
		mockConn.AssertExpectations(t)
	})
}
