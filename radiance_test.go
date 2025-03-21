package radiance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/getlantern/radiance/config"
)

func TestNewRadiance(t *testing.T) {
	// TODO: update tests to reflect current implementation of NewRadiance
	t.Run("it should return a new Radiance instance", func(t *testing.T) {
		r, err := NewRadiance("../", nil)
		assert.NoError(t, err)
		require.NotNil(t, r)
		assert.NotNil(t, r.confHandler)
		assert.False(t, r.connected)
		assert.NotNil(t, r.statusMutex)
	})
}

func TestGetActiveServer(t *testing.T) {
	var tests = []struct {
		name   string
		setup  func(*gomock.Controller) *Radiance
		assert func(*testing.T, *Server, error)
	}{
		{
			name: "it should return nil when VPN is disconnected",
			setup: func(ctrl *gomock.Controller) *Radiance {
				r, _ := NewRadiance("../", nil)
				return r
			},
			assert: func(t *testing.T, server *Server, err error) {
				assert.Nil(t, server)
				assert.NoError(t, err)
			},
		},
		{
			name: "it should return error when there is no current config",
			setup: func(ctrl *gomock.Controller) *Radiance {
				r, err := NewRadiance("../", nil)
				assert.NoError(t, err)
				r.connected = true
				return r
			},
			assert: func(t *testing.T, server *Server, err error) {
				assert.Nil(t, server)
				assert.Error(t, err)
			},
		},
		{
			name: "it should return the active server when VPN is connected",
			setup: func(ctrl *gomock.Controller) *Radiance {
				r, err := NewRadiance("../", nil)
				assert.NoError(t, err)
				r.connected = true
				r.activeConfig.Store(&config.Config{
					Addr:     "1.2.3.4",
					Protocol: "random",
					Location: &config.ProxyConnectConfig_ProxyLocation{City: "new york"},
				})
				return r
			},
			assert: func(t *testing.T, server *Server, err error) {
				assert.NoError(t, err)
				require.NotNil(t, server)
				assert.Equal(t, "1.2.3.4", server.Address)
				assert.Equal(t, "random", server.Protocol)
				assert.Equal(t, "new york", server.Location.City)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			r := tt.setup(ctrl)
			server, err := r.GetActiveServer()
			tt.assert(t, server, err)
		})
	}
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
			r, err := NewRadiance("../", nil)
			require.NoError(t, err)
			err = r.ReportIssue(tt.email, tt.report)
			tt.assert(t, err)
		})
	}
}
