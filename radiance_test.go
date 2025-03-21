package radiance

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/getlantern/radiance/config"
)

func TestNewRadiance(t *testing.T) {
	// TODO: update tests to reflect current implementation of NewRadiance
	t.Run("it should return a new Radiance instance", func(t *testing.T) {
		r, err := NewRadiance(nil)
		assert.NoError(t, err)
		require.NotNil(t, r)
		assert.NotNil(t, r.confHandler)
		assert.False(t, r.connected)
		assert.NotNil(t, r.statusMutex)
	})
}

func TestStartVPN(t *testing.T) {
	t.Skip("TODO: update tests to reflect current implementation of StartVPN")
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
				r, err := NewRadiance(nil)
				assert.NoError(t, err)
				r.confHandler = configHandler
				configHandler.EXPECT().GetConfig(gomock.Any()).Return(nil, "", assert.AnError)

				return r
			},
			assert: func(t *testing.T, r *Radiance, err error) {
				t.Skip("TODO: currently we're not fetching config with sing-box, this test need to be updated")
				assert.Error(t, err)
				assert.False(t, r.connectionStatus())
			},
		},
		{
			name: "it should return an error when providing an invalid config",
			setup: func(ctrl *gomock.Controller) *Radiance {
				configHandler := NewMockconfigHandler(ctrl)
				r, err := NewRadiance(nil)
				assert.NoError(t, err)
				r.confHandler = configHandler
				configHandler.EXPECT().GetConfig(gomock.Any()).Return([]*config.Config{{}}, "", nil)

				return r
			},
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.Error(t, err)
				assert.False(t, r.connectionStatus())
			},
		},
		{
			name: "it should succeed when providing valid config and address",
			setup: func(ctrl *gomock.Controller) *Radiance {
				configHandler := NewMockconfigHandler(ctrl)
				r, err := NewRadiance(nil)
				assert.NoError(t, err)
				r.confHandler = configHandler
				// expect to get config twice, once for the initial check and once for active proxy location
				configHandler.EXPECT().GetConfig(gomock.Any()).Return([]*config.Config{{
					Protocol: "logger",
					Location: &config.ProxyConnectConfig_ProxyLocation{City: "new york"},
				}}, "US", nil).Times(1)

				return r
			},
			givenAddr: "127.0.0.1:6666",
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.NoError(t, err)
				assert.True(t, r.connectionStatus())
				// TODO: update assert when using TUN
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
				errChan <- r.run(tt.givenAddr)
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
	t.Skip("TODO: update tests to reflect current implementation of StopVPN")
	var tests = []struct {
		name   string
		setup  func(*gomock.Controller) *Radiance
		assert func(*testing.T, *Radiance, error)
	}{
		{
			name: "it should return nil when VPN is disconnected",
			setup: func(ctrl *gomock.Controller) *Radiance {
				r, _ := NewRadiance(nil)
				return r
			},
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.NoError(t, err)
			},
		},
		{
			name: "it should return an error when http server is nil",
			setup: func(ctrl *gomock.Controller) *Radiance {
				r, err := NewRadiance(nil)
				assert.NoError(t, err)
				r.connected = true
				return r
			},
			assert: func(t *testing.T, r *Radiance, err error) {
				assert.Error(t, err)
			},
		},
		{
			name: "it should return an error when failed to shutdown http server",
			setup: func(ctrl *gomock.Controller) *Radiance {
				server := NewMockhttpServer(ctrl)
				r, err := NewRadiance(nil)
				assert.NoError(t, err)
				r.connected = true
				r.srv = server
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
				r, err := NewRadiance(nil)
				assert.NoError(t, err)
				r.confHandler = configHandler
				r.connected = true
				r.srv = server
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
			err := r.shutdown(ctx)
			tt.assert(t, r, err)
		})
	}
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
				r, _ := NewRadiance(nil)
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
				r, err := NewRadiance(nil)
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
				r, err := NewRadiance(nil)
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
			r, err := NewRadiance(nil)
			require.NoError(t, err)
			err = r.ReportIssue(tt.email, tt.report)
			tt.assert(t, err)
		})
	}
}
