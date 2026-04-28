package vpn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"

	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/clashapi/trafficontrol"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
)

var _ adapter.ClashServer = (*clashServer)(nil)

// clashServer is a stub adapter.ClashServer: it exposes the traffic manager
// and URL-test history hook the rest of the tunnel depends on, but does not
// run the Clash HTTP API. Start and Close are no-ops because there are no
// owned resources beyond what's wired in via the sing-box service context.
type clashServer struct {
	ctx       context.Context
	dnsRouter adapter.DNSRouter
	outbound  adapter.OutboundManager
	endpoint  adapter.EndpointManager

	urlTestHistory adapter.URLTestHistoryStorage
	trafficManager *trafficontrol.Manager

	mode     string
	modeList []string

	mu sync.RWMutex
}

func newClashServer(ctx context.Context, _ log.ObservableFactory, options option.ClashAPIOptions) (adapter.ClashServer, error) {
	modeList := options.ModeList
	initial := options.DefaultMode
	if len(modeList) == 0 {
		return nil, errors.New("mode list is empty")
	}
	if initial == "" {
		initial = modeList[0]
	} else if !slices.Contains(modeList, initial) {
		return nil, fmt.Errorf("initial mode %q is not in mode list", initial)
	}

	return &clashServer{
		dnsRouter:      service.FromContext[adapter.DNSRouter](ctx),
		outbound:       service.FromContext[adapter.OutboundManager](ctx),
		endpoint:       service.FromContext[adapter.EndpointManager](ctx),
		urlTestHistory: service.FromContext[adapter.URLTestHistoryStorage](ctx),
		trafficManager: trafficontrol.NewManager(),
		modeList:       modeList,
		mode:           initial,
	}, nil
}

func (s *clashServer) SetMode(mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.modeList, func(m string) bool {
		return strings.EqualFold(m, mode)
	})
	if i == -1 {
		return fmt.Errorf("mode %q is not in mode list", mode)
	}
	mode = s.modeList[i]
	if s.mode != mode {
		slog.Info("Switching mode", "from", s.mode, "to", mode)
		s.mode = mode
		s.dnsRouter.ClearCache()
	}
	return nil
}

func (s *clashServer) Mode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

func (s *clashServer) ModeList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.modeList
}

func (s *clashServer) Start(stage adapter.StartStage) error {
	return nil
}

func (s *clashServer) Close() error {
	return nil
}

func (s *clashServer) HistoryStorage() adapter.URLTestHistoryStorage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.urlTestHistory
}

func (s *clashServer) TrafficManager() *trafficontrol.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.trafficManager
}

func (s *clashServer) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	return trafficontrol.NewTCPTracker(conn, s.trafficManager, metadata, s.outbound, matchedRule, matchOutbound)
}

func (s *clashServer) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	return trafficontrol.NewUDPTracker(conn, s.trafficManager, metadata, s.outbound, matchedRule, matchOutbound)
}

func (s *clashServer) Name() string {
	return "clash"
}
