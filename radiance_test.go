package radiance

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/client"
	boxservice "github.com/getlantern/radiance/client/service"
	"github.com/getlantern/radiance/config"
)

func TestNewRadiance(t *testing.T) {
	t.Run("it should create a new Radiance instance successfully", func(t *testing.T) {
		dir := t.TempDir()
		r, err := NewRadiance(client.Options{
			DataDir: dir,
			Locale:  "en-US",
		})
		assert.NoError(t, err)
		defer r.Close()
		assert.NotNil(t, r)
		assert.NotNil(t, r.VPNClient)
		assert.NotNil(t, r.confHandler)
		assert.NotNil(t, r.activeServer)
		assert.NotNil(t, r.stopChan)
		assert.NotNil(t, r.user)
		assert.NotNil(t, r.issueReporter)
	})
}

type mockVPNClient struct {
	mock.Mock
}

func (m *mockVPNClient) StartVPN() error                         { panic("not implemented") }
func (m *mockVPNClient) StopVPN() error                          { panic("not implemented") }
func (m *mockVPNClient) PauseVPN(dur time.Duration) error        { return nil }
func (m *mockVPNClient) ResumeVPN()                              {}
func (m *mockVPNClient) SplitTunnelHandler() *client.SplitTunnel { panic("not implemented") }
func (m *mockVPNClient) OnNewConfig(oldConfig, newConfig *config.Config) error {
	return nil
}
func (m *mockVPNClient) ParseConfig(config []byte) (*config.Config, error) {
	return nil, nil
}

func (m *mockVPNClient) ConnectionStatus() bool {
	args := m.Called()
	return args.Bool(0)
}
func (m *mockVPNClient) GetActiveServer() (*Server, error) {
	args := m.Called()
	return args.Get(0).(*Server), args.Error(1)
}

func (m *mockVPNClient) AddCustomServer(cfg boxservice.ServerConnectConfig) error {
	return nil
}
func (m *mockVPNClient) SelectCustomServer(tag string) error {
	return nil
}
func (m *mockVPNClient) RemoveCustomServer(tag string) error {
	return nil
}

func TestReportIssue(t *testing.T) {
	var tests = []struct {
		name   string
		email  string
		report IssueReport
		assert func(*testing.T, error)
	}{
		{
			name:   "it should return error when issue report is missing both type and description",
			email:  "",
			report: IssueReport{},
			assert: func(t *testing.T, err error) {
				assert.Error(t, err)
			},
		},
		{
			name:  "it should return nil when issue report is valid",
			email: "radiancetest@getlantern.org",
			report: IssueReport{
				Type:        "Application crashes",
				Description: "internal test only",
				Device:      "test device",
				Model:       "a123",
			},
			assert: func(t *testing.T, err error) {
				assert.NoError(t, err)
			},
		},
		{
			name:  "it should return nil when issue report is valid with empty email",
			email: "",
			report: IssueReport{
				Type:        "Cannot sign in",
				Description: "internal test only",
				Device:      "test device 2",
				Model:       "b456",
			},
			assert: func(t *testing.T, err error) {
				assert.NoError(t, err)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewRadiance(client.Options{DataDir: t.TempDir()})
			defer r.Close()
			require.NoError(t, err)
			err = r.ReportIssue(tt.email, &tt.report)
			tt.assert(t, err)
		})
	}
}

func TestGetActiveServer(t *testing.T) {
	t.Run("it should return error when VPN is not connected", func(t *testing.T) {
		mockClient := &mockVPNClient{}
		mockClient.On("ConnectionStatus").Return(false)

		r := &Radiance{
			VPNClient:    mockClient,
			activeServer: new(atomic.Value),
		}

		server, err := r.GetActiveServer()
		assert.Nil(t, server)
		assert.Error(t, err)
		assert.EqualError(t, err, "VPN is not connected")
	})

	t.Run("it should return error when no active server config is available", func(t *testing.T) {
		mockClient := &mockVPNClient{}
		mockClient.On("ConnectionStatus").Return(true)

		r := &Radiance{
			VPNClient:    mockClient,
			activeServer: new(atomic.Value),
		}

		server, err := r.GetActiveServer()
		assert.Nil(t, server)
		assert.Error(t, err)
		assert.EqualError(t, err, "no active server config")
	})

	t.Run("it should return the active server when VPN is connected and active server is set", func(t *testing.T) {
		mockClient := &mockVPNClient{}
		mockClient.On("ConnectionStatus").Return(true)

		activeServer := &Server{
			Address:  "127.0.0.1",
			Location: C.ServerLocation{Country: "US", City: "New York"},
			Protocol: "tcp",
		}

		r := &Radiance{
			VPNClient:    mockClient,
			activeServer: new(atomic.Value),
		}
		r.activeServer.Store(activeServer)

		server, err := r.GetActiveServer()
		assert.NotNil(t, server)
		assert.NoError(t, err)
		assert.Equal(t, activeServer, server)
	})
}
