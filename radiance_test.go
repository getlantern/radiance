package radiance

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/config"
)

func TestNewRadiance(t *testing.T) {
	t.Run("it should create a new Radiance instance successfully", func(t *testing.T) {
		r, err := NewRadiance(t.TempDir(), nil)
		assert.NotNil(t, r)
		assert.NoError(t, err)
		assert.NotNil(t, r.VPNClient)
		assert.NotNil(t, r.confHandler)
		assert.NotNil(t, r.activeConfig)
		assert.NotNil(t, r.stopChan)
		assert.NotNil(t, r.user)
		assert.NotNil(t, r.issueReporter)
	})
}

func TestGetActiveServer(t *testing.T) {
	var tests = []struct {
		name      string
		want      *Server
		setup     func(*Server) *Radiance
		assertErr func(assert.TestingT, error, ...interface{}) bool
	}{
		{
			name: "it should return nil when VPN is disconnected",
			setup: func(*Server) *Radiance {
				vpn := &mockVPNClient{}
				vpn.On("ConnectionStatus").Return(false)
				return &Radiance{VPNClient: vpn}
			},
			assertErr: assert.NoError,
		},
		{
			name: "it should return error when there is no current config",
			setup: func(*Server) *Radiance {
				vpn := &mockVPNClient{}
				vpn.On("ConnectionStatus").Return(true)
				return &Radiance{
					VPNClient:    vpn,
					activeConfig: &atomic.Value{},
				}
			},
			assertErr: assert.Error,
		},
		{
			name: "it should return the active server when VPN is connected",
			want: &Server{
				Address:  "1.2.3.4",
				Protocol: "random",
				Location: ServerLocation{City: "new york"},
			},
			setup: func(s *Server) *Radiance {
				vpn := &mockVPNClient{}
				vpn.On("ConnectionStatus").Return(true)
				r := &Radiance{
					VPNClient:    vpn,
					activeConfig: &atomic.Value{},
				}
				c := &config.Config{
					Addr:     s.Address,
					Location: &config.ProxyConnectConfig_ProxyLocation{City: s.Location.City},
					Protocol: s.Protocol,
				}
				r.activeConfig.Store(c)
				return r
			},
			assertErr: assert.NoError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.setup(tt.want)
			server, err := r.GetActiveServer()
			assert.Equal(t, tt.want, server)
			tt.assertErr(t, err)
		})
	}
}

type mockVPNClient struct {
	mock.Mock
}

func (m *mockVPNClient) StartVPN() error                  { panic("not implemented") }
func (m *mockVPNClient) StopVPN() error                   { panic("not implemented") }
func (m *mockVPNClient) PauseVPN(dur time.Duration) error { return nil }
func (m *mockVPNClient) ResumeVPN()                       {}

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
			r, err := NewRadiance(t.TempDir(), nil)
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
