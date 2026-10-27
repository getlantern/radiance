package vpn

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/servers"
)

func TestConfigRefreshPreservesDNSDetour(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	started := make(chan struct{})
	resolver := &dns.Server{
		Listener:          listener,
		NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
			reply := new(dns.Msg).SetReply(query)
			reply.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET},
				A:   net.ParseIP("192.0.2.1"),
			}}
			_ = w.WriteMsg(reply)
		}),
	}
	go func() { _ = resolver.ActivateAndServe() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("DNS server did not start")
	}
	t.Cleanup(func() { assert.NoError(t, resolver.Shutdown()) })
	options := O.Options{DNS: &O.DNSOptions{RawDNSOptions: O.RawDNSOptions{
		Servers: []O.DNSServerOptions{{Tag: "resolver", Type: C.DNSTypeTCP, Options: &O.RemoteDNSServerOptions{
			RawLocalDNSServerOptions: O.RawLocalDNSServerOptions{DialerOptions: O.DialerOptions{Detour: "auxiliary"}},
			DNSServerAddressOptions:  O.DNSServerAddressOptions{Server: "127.0.0.1", ServerPort: uint16(listener.Addr().(*net.TCPAddr).Port)},
		}}},
		Final: "resolver",
	}}}
	tun := startAuxiliaryDetourTunnel(t, options)
	router := service.FromContext[adapter.DNSRouter](tun.ctx)
	exchange := func() {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		reply, err := router.Exchange(ctx, new(dns.Msg).SetQuestion("retained.example.", dns.TypeA), adapter.DNSQueryOptions{DisableCache: true})
		require.NoError(t, err)
		require.Len(t, reply.Answer, 1)
		assert.Equal(t, "192.0.2.1", reply.Answer[0].(*dns.A).A.String())
	}
	exchange()
	original, found := tun.outboundMgr.Outbound("auxiliary")
	require.True(t, found)
	replacement := servers.ServerList{Servers: []*servers.Server{
		{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{
			Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
		}},
		{Tag: "fresh", Type: C.TypeDirect, Options: O.Outbound{
			Tag: "fresh", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
		}},
		{Tag: "auxiliary", Type: lbC.TypeMutableSelector, Options: O.Outbound{
			Tag: "auxiliary", Type: lbC.TypeMutableSelector, Options: &lbO.MutableSelectorOutboundOptions{Outbounds: []string{"working"}},
		}},
	}}
	require.ErrorContains(t, tun.updateConfigOutbounds(replacement, servers.ServerList{}), "reconnect")
	current, found := tun.outboundMgr.Outbound("auxiliary")
	require.True(t, found)
	assert.Same(t, original, current)
	assert.Equal(t, []string{"helper"}, current.(adapter.OutboundGroup).All())
	_, found = tun.outboundMgr.Outbound("fresh")
	assert.False(t, found)
	exchange()
	retireAuxiliaryDetour(t, tun)
	exchange()
	restoreAuxiliaryDetour(t, tun)
	exchange()
}

func TestConfigRefreshPreservesRuleSetDownloadDetour(t *testing.T) {
	var requests atomic.Int32
	rules := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		_, _ = w.Write([]byte(`{"version":3,"rules":[{"domain":["example.com"]}]}`))
		requests.Add(1)
	}))
	t.Cleanup(rules.Close)
	options := O.Options{Route: &O.RouteOptions{RuleSet: []O.RuleSet{{
		Type: C.RuleSetTypeRemote, Tag: "remote", Format: C.RuleSetFormatSource,
		RemoteOptions: O.RemoteRuleSet{URL: rules.URL, DownloadDetour: "auxiliary", UpdateInterval: badoption.Duration(50 * time.Millisecond)},
	}}}}
	tun := startAuxiliaryDetourTunnel(t, options)
	require.Positive(t, requests.Load())
	retireAuxiliaryDetour(t, tun)
	before := requests.Load()
	require.Eventually(t, func() bool { return requests.Load() > before }, time.Second, 10*time.Millisecond)
	restoreAuxiliaryDetour(t, tun)
	before = requests.Load()
	require.Eventually(t, func() bool { return requests.Load() > before }, time.Second, 10*time.Millisecond)
}

func startAuxiliaryDetourTunnel(t *testing.T, options O.Options) *tunnel {
	t.Helper()
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(probe.Close)
	options.Log = &O.LogOptions{Disabled: true}
	options.Outbounds = []O.Outbound{
		{Tag: "helper", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}},
		{Tag: "auxiliary", Type: lbC.TypeMutableSelector, Options: &lbO.MutableSelectorOutboundOptions{Outbounds: []string{"helper"}}},
		{Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}},
		{Tag: ManualSelectTag, Type: lbC.TypeMutableSelector, Options: &lbO.MutableSelectorOutboundOptions{Outbounds: []string{"auxiliary", "working"}}},
		{Tag: AutoSelectTag, Type: lbC.TypeMutableAutoSelect, Options: &lbO.MutableAutoSelectOutboundOptions{Outbounds: []string{"auxiliary", "working"}, URL: probe.URL}},
	}
	if options.Route == nil {
		options.Route = &O.RouteOptions{}
	}
	options.Route.Rules = []O.Rule{selectModeRule(AutoSelectTag), selectModeRule(ManualSelectTag)}
	options.Route.Final = "working"
	options.Experimental = &O.ExperimentalOptions{
		ClashAPI:  &O.ClashAPIOptions{DefaultMode: AutoSelectTag},
		CacheFile: &O.CacheFileOptions{Enabled: true, Path: filepath.Join(t.TempDir(), "cache.db")},
	}
	tun := &tunnel{dataPath: t.TempDir(), initialNonSelectable: []string{"helper"}}
	t.Cleanup(func() { assert.NoError(t, tun.close()) })
	require.NoError(t, tun.init(t.Context(), options, nil))
	require.NoError(t, tun.boxInstance.Start())
	tun.optsMap = makeOutboundOptsMap(tun.ctx, options)
	tun.nonSelectableTags = makeNonSelectableTagSet(tun.initialNonSelectable, options)
	tun.outboundMgr = service.FromContext[adapter.OutboundManager](tun.ctx)
	tun.clashServer = service.FromContext[adapter.ClashServer](tun.ctx).(*clashServer)
	manager, err := newMutableGroupManager(tun.ctx, tun.logFactory.NewLogger("groupsManager"), tun.clashServer.connTracker)
	require.NoError(t, err)
	tun.mutGrpMgr = manager
	t.Cleanup(manager.Close)
	return tun
}

func retireAuxiliaryDetour(t *testing.T, tun *tunnel) {
	t.Helper()
	working := servers.ServerList{Servers: []*servers.Server{{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{
		Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
	}}}}
	for range 2 {
		require.NoError(t, tun.updateConfigOutbounds(working, servers.ServerList{}))
	}
	for _, tag := range []string{ManualSelectTag, AutoSelectTag} {
		group, found := tun.mutGrpMgr.OutboundGroup(tag)
		require.True(t, found)
		assert.Equal(t, []string{"working"}, group.All())
	}
	for _, tag := range []string{"auxiliary", "helper"} {
		_, found := tun.outboundMgr.Outbound(tag)
		assert.True(t, found, tag)
	}
}

func restoreAuxiliaryDetour(t *testing.T, tun *tunnel) {
	t.Helper()
	original, found := tun.outboundMgr.Outbound("auxiliary")
	require.True(t, found)
	list := servers.ServerList{Servers: []*servers.Server{
		{Tag: "working", Type: C.TypeDirect, Options: O.Outbound{
			Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
		}},
		{Tag: "auxiliary", Type: lbC.TypeMutableSelector, Options: O.Outbound{
			Tag: "auxiliary", Type: lbC.TypeMutableSelector, Options: &lbO.MutableSelectorOutboundOptions{Outbounds: []string{"helper"}},
		}},
	}}
	helper := servers.ServerList{Servers: []*servers.Server{{Tag: "helper", Type: C.TypeDirect, Options: O.Outbound{
		Tag: "helper", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{},
	}}}}
	require.NoError(t, tun.updateConfigOutbounds(list, helper))
	current, found := tun.outboundMgr.Outbound("auxiliary")
	require.True(t, found)
	assert.Same(t, original, current)
	assert.NotContains(t, tun.pendingRemovalTags, "auxiliary")
	for _, tag := range []string{ManualSelectTag, AutoSelectTag} {
		group, found := tun.mutGrpMgr.OutboundGroup(tag)
		require.True(t, found)
		assert.ElementsMatch(t, []string{"auxiliary", "working"}, group.All())
	}
}
