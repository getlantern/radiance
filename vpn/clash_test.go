package vpn

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	lcommon "github.com/getlantern/common"
	box "github.com/getlantern/lantern-box"
	lbO "github.com/getlantern/lantern-box/option"
	lbgroup "github.com/getlantern/lantern-box/protocol/group"
	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/observable"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClashServerHistoryStorage(t *testing.T) {
	for _, registration := range []string{"none", "interface", "pointer"} {
		for _, started := range []bool{false, true} {
			name := registration + "/unstarted"
			if started {
				name = registration + "/started"
			}
			t.Run(name, func(t *testing.T) {
				ctx := t.Context()
				supplied := urltest.NewHistoryStorage()
				t.Cleanup(func() { assert.NoError(t, supplied.Close()) })
				switch registration {
				case "interface":
					ctx = service.ContextWith[A.URLTestHistoryStorage](ctx, supplied)
				case "pointer":
					ctx = service.ContextWithPtr(ctx, supplied)
				}

				srv, err := newClashServer(ctx, nil, O.ClashAPIOptions{ModeList: []string{"rule"}})
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, srv.Close()) })
				if started {
					require.NoError(t, srv.Start(A.StartStateStart))
				}

				history := srv.HistoryStorage()
				require.NotNil(t, history)
				require.Same(t, history, srv.HistoryStorage())
				if registration != "none" {
					require.Same(t, supplied, history)
				}
				entry := &A.URLTestHistory{Time: time.Now(), Delay: 10}
				history.StoreURLTestHistory("probe", entry)
				assert.Equal(t, entry, history.LoadURLTestHistory("probe"))
				history.DeleteURLTestHistory("probe")
				assert.Nil(t, history.LoadURLTestHistory("probe"))

				hook := observable.NewSubscriber[struct{}](1)
				t.Cleanup(func() { assert.NoError(t, hook.Close()) })
				updates, _ := hook.Subscription()
				history.SetHook(hook)
				history.StoreURLTestHistory("probe", entry)
				require.Len(t, updates, 1)
				<-updates

				require.NoError(t, srv.Close())
				history.StoreURLTestHistory("probe", entry)
				if registration == "none" {
					assert.Empty(t, updates, "closing owned storage must detach its hook")
				} else {
					assert.Len(t, updates, 1, "closing the server must leave borrowed storage open")
				}
			})
		}
	}
}

func TestTunnelSmartRoutingHistory(t *testing.T) {
	tun := &tunnel{dataPath: t.TempDir()}
	var previous A.URLTestHistoryStorage
	for _, cycle := range []string{"connect", "reconnect"} {
		t.Run(cycle, func(t *testing.T) {
			probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(probe.Close)
			options, err := json.UnmarshalExtendedContext[O.Options](box.Context(t.Context()), []byte(`{
				"outbounds": [{"type": "direct", "tag": "probe"}],
				"route": {"rules": [{"clash_mode": "rule", "outbound": "sr-ai-domains"}]},
				"experimental": {"clash_api": {"default_mode": "rule"}}
			}`))
			require.NoError(t, err)
			options.Experimental.CacheFile = &O.CacheFileOptions{Enabled: true, Path: filepath.Join(t.TempDir(), "cache.db")}
			smartRouting := lcommon.SmartRoutingRules{{Category: "ai-domains", Outbounds: []string{"probe"}}}
			outbounds, _, _ := smartRouting.ToOptions(urlTestInterval, urlTestIdleTimeout)
			outbounds[0].Options.(*O.URLTestOutboundOptions).URL = probe.URL
			options.Outbounds = append(options.Outbounds, outbounds...)

			t.Cleanup(func() { assert.NoError(t, tun.close()) })
			require.NoError(t, tun.init(t.Context(), options, nil))
			srv := service.FromContext[A.ClashServer](tun.ctx)
			require.IsType(t, &clashServer{}, srv)
			history := srv.HistoryStorage()
			require.NotNil(t, history)
			if previous != nil {
				require.NotSame(t, previous, history)
			}
			assert.Nil(t, history.LoadURLTestHistory("probe"))
			previous = history

			require.NoError(t, tun.boxInstance.Start())
			require.Eventually(t, func() bool {
				return history.LoadURLTestHistory("probe") != nil
			}, 3*time.Second, time.Millisecond, "startup must record the background URL test")

			mutable, err := lbgroup.NewMutableURLTest(tun.ctx, nil, tun.logFactory.Logger(), "mutable", lbO.MutableURLTestOutboundOptions{
				Outbounds: []string{"probe"},
				URL:       probe.URL,
			})
			require.NoError(t, err)
			mutableGroup := mutable.(*lbgroup.MutableURLTest)
			t.Cleanup(func() { assert.NoError(t, mutableGroup.Close()) })
			require.NoError(t, mutableGroup.Start())
			native, found := service.FromContext[A.OutboundManager](tun.ctx).Outbound("sr-ai-domains")
			require.True(t, found)
			groups := []struct {
				name  string
				group interface{ CheckOutbounds() }
			}{
				{"native", native.(interface{ CheckOutbounds() })},
				{"mutable", mutableGroup},
			}
			for _, group := range groups {
				t.Run(group.name+"/success", func(t *testing.T) {
					stale := &A.URLTestHistory{Time: time.Now().Add(-time.Hour), Delay: 1}
					history.StoreURLTestHistory("probe", stale)
					require.Eventually(t, func() bool {
						group.group.CheckOutbounds()
						entry := history.LoadURLTestHistory("probe")
						return entry != nil && entry != stale
					}, 3*time.Second, time.Millisecond, "URL tests must update the shared history")
				})
			}

			probe.Close()
			for _, group := range groups {
				t.Run(group.name+"/failure", func(t *testing.T) {
					history.StoreURLTestHistory("probe", &A.URLTestHistory{Time: time.Now(), Delay: 1})
					require.Eventually(t, func() bool {
						group.group.CheckOutbounds()
						return history.LoadURLTestHistory("probe") == nil
					}, 3*time.Second, time.Millisecond, "failed URL tests must remove stale history")
				})
			}
		})
	}
}
