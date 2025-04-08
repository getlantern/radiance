package radiance

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	C "github.com/getlantern/common"
	"github.com/getlantern/radiance/client"
	"github.com/getlantern/radiance/config"
)

func TestNewRadiance(t *testing.T) {
	t.Run("it should create a new Radiance instance successfully", func(t *testing.T) {
		r, err := NewRadiance(client.Options{DataDir: t.TempDir()})
		assert.NotNil(t, r)
		assert.NoError(t, err)
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
			require.NoError(t, err)
			err = r.ReportIssue(tt.email, &tt.report)
			tt.assert(t, err)
		})
	}
}
func TestSetupDirs(t *testing.T) {
	t.Run("it should return default directories when baseDir is empty", func(t *testing.T) {
		dataDir, logDir, err := setupDirs("")
		assert.NoError(t, err)
		assert.NotEmpty(t, dataDir)
		assert.NotEmpty(t, logDir)
	})

	t.Run("it should create and return directories when baseDir is provided", func(t *testing.T) {
		baseDir := t.TempDir()
		dataDir, logDir, err := setupDirs(baseDir)
		assert.NoError(t, err)
		assert.Equal(t, baseDir, dataDir)
		assert.Equal(t, filepath.Join(baseDir, "logs"), logDir)
		assert.DirExists(t, logDir)
	})

	t.Run("it should return error when it fails to create directories", func(t *testing.T) {
		baseDir := "/invalid/path"
		_, _, err := setupDirs(baseDir)
		assert.Error(t, err)
	})
}
func TestGetActiveServer(t *testing.T) {
	t.Run("it should return nil when VPN is disconnected", func(t *testing.T) {
		mockClient := &mockVPNClient{}
		mockClient.On("ConnectionStatus").Return(false)

		r := &Radiance{
			VPNClient:    mockClient,
			activeServer: new(atomic.Value),
		}

		server, err := r.GetActiveServer()
		assert.NoError(t, err)
		assert.Nil(t, server)
		mockClient.AssertCalled(t, "ConnectionStatus")
	})

	t.Run("it should return error when no active server config is available", func(t *testing.T) {
		mockClient := &mockVPNClient{}
		mockClient.On("ConnectionStatus").Return(true)

		r := &Radiance{
			VPNClient:    mockClient,
			activeServer: new(atomic.Value),
		}

		server, err := r.GetActiveServer()
		assert.Error(t, err)
		assert.Nil(t, server)
		assert.EqualError(t, err, "no active server config")
		mockClient.AssertCalled(t, "ConnectionStatus")
	})

	t.Run("it should return the active server when VPN is connected", func(t *testing.T) {
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
		assert.NoError(t, err)
		assert.NotNil(t, server)
		assert.Equal(t, activeServer, server)
		mockClient.AssertCalled(t, "ConnectionStatus")
	})
}
