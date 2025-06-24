package boxservice

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qdm12/reprint"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/common"
	sbx "github.com/getlantern/sing-box-extensions"

	"github.com/getlantern/radiance/client/boxoptions"
	"github.com/getlantern/radiance/internal"
)

func TestSelectServer_NotRunning(t *testing.T) {
	testServerInfo := func(tag string) CustomServerInfo {
		return CustomServerInfo{
			Tag: tag,
			Outbound: &option.Outbound{
				Tag:     tag,
				Type:    "direct",
				Options: option.DirectOutboundOptions{},
			},
		}
	}
	tests := []struct {
		name      string
		tag       string
		serverMgr *CustomServerManager
		shouldErr bool
	}{
		{
			name:      "EmptyTag",
			tag:       "",
			shouldErr: true,
		},
		{
			name:      "UserServerManagerNil",
			tag:       "tag1",
			shouldErr: true,
		},
		{
			name: "ServerNotFound",
			tag:  "tag1",
			serverMgr: &CustomServerManager{
				customServers: map[string]CustomServerInfo{},
			},
			shouldErr: true,
		},
		{
			name: "Success",
			tag:  "tag1",
			serverMgr: &CustomServerManager{
				customServers: map[string]CustomServerInfo{
					"tag1": testServerInfo("tag1"),
				},
			},
			shouldErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{Name: "default"}
			bs := &BoxService{
				ctx:               context.Background(),
				activeServer:      atomic.Value{},
				userServerManager: tt.serverMgr,
			}
			bs.activeServer.Store(server)

			err := bs.SelectServer(boxoptions.ServerGroupUser, tt.tag)
			got := bs.activeServer.Load().(*Server)
			if tt.shouldErr {
				assert.Error(t, err)
				assert.Equal(t, server, got)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.tag, got.Name)
			}
		})
	}
}

func TestSetInitialServer(t *testing.T) {
	opts := boxoptions.BoxOptions
	server := &Server{
		Name:  "want",
		Group: boxoptions.ServerGroupUser,
	}
	outs := []option.Outbound{
		{
			Type: constant.TypeDirect,
			Tag:  "init",
		},
		{
			Type:    constant.TypeHTTP,
			Tag:     server.Name,
			Options: &option.HTTPOutboundOptions{},
		},
	}
	opts.Outbounds = append(opts.Outbounds, outs...)
	sopts, err := findSelector(opts.Outbounds, server.Group)
	require.NoError(t, err)

	sopts.Outbounds = []string{outs[0].Tag, outs[1].Tag}
	sopts.Default = outs[0].Tag

	prevClashMode := opts.Experimental.ClashAPI.DefaultMode
	if assert.NoError(t, setInitialServer(opts, server)) {
		assert.Equal(t, server.Name, sopts.Default)
		assert.Equal(t, server.Group, opts.Experimental.ClashAPI.DefaultMode)
	} else {
		assert.Equal(t, sopts.Default, outs[0].Tag)
		assert.Equal(t, prevClashMode, opts.Experimental.ClashAPI.DefaultMode)
	}
}

func TestInsertUserServers(t *testing.T) {
	opts := boxoptions.BoxOptions
	outs := []option.Outbound{
		{
			Type: constant.TypeDirect,
			Tag:  "server1",
		},
		{
			Type:    constant.TypeHTTP,
			Tag:     "server2",
			Options: &option.HTTPOutboundOptions{},
		},
	}
	servers := []CustomServerInfo{
		{Tag: outs[0].Tag, Outbound: &outs[0]},
		{Tag: outs[1].Tag, Outbound: &outs[1]},
	}

	insertUserServers(&opts, servers)
	for _, server := range servers {
		assert.Contains(t, opts.Outbounds, *server.Outbound, "Expected outbound %s to be inserted", server.Tag)
	}
	sopts, err := findSelector(opts.Outbounds, boxoptions.ServerGroupUser)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{outs[0].Tag, outs[1].Tag}, sopts.Outbounds, "selector outbound should include all user server tags")
}

func TestReloadOptions(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test-opts.json")
	writeConfig := func(config common.ConfigResponse) {
		buf, err := json.MarshalContext(BaseContext(), config)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, buf, 0644))
	}

	baseOptions := boxoptions.BoxOptions
	assertFn := func(newOptions, got option.Options) {
		want := reprint.This(baseOptions).(option.Options)
		want.Outbounds = append(want.Outbounds, newOptions.Outbounds...)
		got.RawMessage = nil
		assert.Equal(t, want, got)
	}

	confResp := common.ConfigResponse{
		Options: option.Options{
			Outbounds: []option.Outbound{
				{
					Type: constant.TypeHTTP,
					Tag:  "http-out",
					Options: &option.HTTPOutboundOptions{
						ServerOptions: option.ServerOptions{
							Server:     "1.1.1.1",
							ServerPort: 80,
						},
					},
				},
			},
		},
	}
	writeConfig(confResp)

	bs := &BoxService{
		ctx:        sbx.BoxContext(),
		isRunning:  false,
		mu:         sync.Mutex{},
		configPath: path,
		options:    baseOptions,
	}

	require.NoError(t, bs.reloadOptions())
	assertFn(confResp.Options, bs.options)

	watcher := internal.NewFileWatcher(path, func() {
		require.NoError(t, bs.reloadOptions())
	})
	bs.optsFileWatcher = watcher
	require.NoError(t, watcher.Start())
	defer watcher.Close()

	confResp.Options.Outbounds = []option.Outbound{
		{
			Type: constant.TypeSOCKS,
			Tag:  "socks-out",
			Options: &option.SOCKSOutboundOptions{
				ServerOptions: option.ServerOptions{
					Server:     "0.0.0.0",
					ServerPort: 8080,
				},
				Version: "5",
			},
		},
	}
	writeConfig(confResp)
	time.Sleep(150 * time.Millisecond) // wait for watcher to trigger
	assertFn(confResp.Options, bs.options)
}

func TestUpdateOutbounds(t *testing.T) {
	tests := []struct {
		name      string
		outbounds []adapter.Outbound
		updates   []option.Outbound
		exclude   []string
		want      []adapter.Outbound
		error     bool
	}{
		{
			name: "upsert outbounds",
			outbounds: []adapter.Outbound{
				&mockEndpoint{tag: "tag1", typ: "type1"},
				&mockEndpoint{tag: "tag2", typ: "type2"},
				&mockEndpoint{tag: "tag3", typ: "type3"},
			},
			updates: []option.Outbound{
				{Tag: "tag1", Type: "NewType"},
				{Tag: "tag4", Type: "NewType"},
			},
			want: []adapter.Outbound{
				&mockEndpoint{tag: "tag1", typ: "NewType"},
				&mockEndpoint{tag: "tag4", typ: "NewType"},
			},
		},
		{
			name: "exclude outbounds",
			outbounds: []adapter.Outbound{
				&mockEndpoint{tag: "direct", typ: "type1"},
				&mockEndpoint{tag: "dns", typ: "type1"},
				&mockEndpoint{tag: "tag2", typ: "type2"},
			},
			updates: []option.Outbound{
				{Tag: "direct", Type: "NewType"},
				{Tag: "tag4", Type: "NewType"},
			},
			exclude: []string{"direct", "dns"},
			want: []adapter.Outbound{
				&mockEndpoint{tag: "direct", typ: "type1"},
				&mockEndpoint{tag: "dns", typ: "type1"},
				&mockEndpoint{tag: "tag4", typ: "NewType"},
			},
		},
		{
			name: "update valid and return error for missing tag",
			outbounds: []adapter.Outbound{
				&mockEndpoint{tag: "tag2", typ: "type2"},
			},
			updates: []option.Outbound{
				{Tag: "tag2", Type: "NewType"},
				{Tag: "", Type: "NewType"},
			},
			want: []adapter.Outbound{
				&mockEndpoint{tag: "tag2", typ: "NewType"},
			},
			error: true,
		},
	}
	for _, tt := range tests {
		ctx := context.Background()
		logger := sblog.NewNOPFactory()
		t.Run(tt.name, func(t *testing.T) {
			mgr := mockOutboundManager{outbounds: tt.outbounds}
			err := updateOutbounds(ctx, &mgr, nil, logger, tt.updates, tt.exclude)
			if tt.error {
				assert.Error(t, err)
			}
			got := mgr.Outbounds()
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestUpdateEndpoints(t *testing.T) {
	tests := []struct {
		name      string
		endpoints []adapter.Endpoint
		updates   []option.Endpoint
		exclude   []string
		want      []adapter.Endpoint
		error     bool
	}{
		{
			name: "upsert endpoints",
			endpoints: []adapter.Endpoint{
				&mockEndpoint{tag: "tag1", typ: "type1"},
				&mockEndpoint{tag: "tag2", typ: "type2"},
				&mockEndpoint{tag: "tag3", typ: "type3"},
			},
			updates: []option.Endpoint{
				{Tag: "tag1", Type: "NewType"},
				{Tag: "tag4", Type: "NewType"},
			},
			want: []adapter.Endpoint{
				&mockEndpoint{tag: "tag1", typ: "NewType"},
				&mockEndpoint{tag: "tag4", typ: "NewType"},
			},
		},
		{
			name: "exclude endpoints",
			endpoints: []adapter.Endpoint{
				&mockEndpoint{tag: "direct", typ: "type1"},
				&mockEndpoint{tag: "dns", typ: "type1"},
				&mockEndpoint{tag: "tag2", typ: "type2"},
			},
			updates: []option.Endpoint{
				{Tag: "direct", Type: "NewType"},
				{Tag: "tag4", Type: "NewType"},
			},
			exclude: []string{"direct", "dns"},
			want: []adapter.Endpoint{
				&mockEndpoint{tag: "direct", typ: "type1"},
				&mockEndpoint{tag: "dns", typ: "type1"},
				&mockEndpoint{tag: "tag4", typ: "NewType"},
			},
		},
		{
			name: "update valid and return error for missing tag",
			endpoints: []adapter.Endpoint{
				&mockEndpoint{tag: "tag2", typ: "type2"},
			},
			updates: []option.Endpoint{
				{Tag: "tag2", Type: "NewType"},
				{Tag: "", Type: "NewType"},
			},
			want: []adapter.Endpoint{
				&mockEndpoint{tag: "tag2", typ: "NewType"},
			},
			error: true,
		},
	}
	for _, tt := range tests {
		ctx := context.Background()
		logger := sblog.NewNOPFactory()
		t.Run(tt.name, func(t *testing.T) {
			mgr := mockEndpointManager{endpoints: tt.endpoints}
			err := updateEndpoints(ctx, &mgr, nil, logger, tt.updates, tt.exclude)
			if tt.error {
				assert.Error(t, err)
			}
			got := mgr.Endpoints()
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

type mockOutboundManager struct {
	adapter.OutboundManager
	outbounds []adapter.Outbound
}

func (m *mockOutboundManager) Outbound(tag string) (adapter.Outbound, bool) {
	for _, outbound := range m.outbounds {
		if outbound.Tag() == tag {
			return outbound, true
		}
	}
	return nil, false
}

func (m *mockOutboundManager) Outbounds() []adapter.Outbound {
	return m.outbounds
}

func (m *mockOutboundManager) Remove(tag string) error {
	m.outbounds = testRemove(m.outbounds, tag)
	return nil
}

func (m *mockOutboundManager) Create(ctx context.Context, router adapter.Router, logger sblog.ContextLogger, tag, outboundType string, options any) error {
	m.Remove(tag)
	outbound := &mockEndpoint{typ: outboundType, tag: tag}
	m.outbounds = append(m.outbounds, outbound)

	for _, stage := range adapter.ListStartStages {
		if err := adapter.LegacyStart(outbound, stage); err != nil {
			return err
		}
	}
	return nil
}

type mockEndpointManager struct {
	adapter.EndpointManager
	endpoints []adapter.Endpoint
}

func (m *mockEndpointManager) Endpoints() []adapter.Endpoint {
	return m.endpoints
}

func (m *mockEndpointManager) Remove(tag string) error {
	m.endpoints = testRemove(m.endpoints, tag)
	return nil
}

func (m *mockEndpointManager) Create(ctx context.Context, router adapter.Router, logger sblog.ContextLogger, tag string, typ string, options interface{}) error {
	m.Remove(tag)
	m.endpoints = append(m.endpoints, &mockEndpoint{typ: typ, tag: tag})
	return nil
}

func testRemove[T adapter.Outbound](list []T, tag string) []T {
	idx := slices.IndexFunc(list, func(e T) bool {
		return e.Tag() == tag
	})
	if idx == -1 {
		return list
	}
	return slices.Delete(list, idx, idx+1)
}

type mockEndpoint struct {
	adapter.Endpoint
	typ              string
	tag              string
	selectedOutbound string
}

func (m *mockEndpoint) Start(stage adapter.StartStage) error {
	return nil
}

func (m *mockEndpoint) SelectOutbound(tag string) bool {
	m.selectedOutbound = tag
	return true
}

func (m *mockEndpoint) All() []string {
	return []string{m.tag}
}

func (m *mockEndpoint) Now() string {
	return m.selectedOutbound
}

func (m *mockEndpoint) Type() string { return m.typ }
func (m *mockEndpoint) Tag() string  { return m.tag }
