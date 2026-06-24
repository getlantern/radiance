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
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"

	"github.com/gofrs/uuid/v5"
)

const rejectMode = "reject"

type dialAdmissionGate interface {
	RecordDial(time.Time)
}

var _ A.ClashServer = (*clashServer)(nil)

// clashServer is a stub A.ClashServer: it exposes the traffic manager
// and URL-test history hook the rest of the tunnel depends on, but does not
// run the Clash HTTP API. Start and Close are no-ops because there are no
// owned resources beyond what's wired in via the sing-box service context.
type clashServer struct {
	ctx       context.Context
	cancel    context.CancelFunc
	startOnce sync.Once

	dnsRouter A.DNSRouter
	outbound  A.OutboundManager
	endpoint  A.EndpointManager

	connTracker       *connTracker
	throughputTracker *throughputTracker
	trackerDone       chan struct{}

	mode     string
	modeList []string

	admissionGate atomic.Value // dialAdmissionGate; set once before the box routes, read lock-free on the dial path
	rejecting     atomic.Bool

	mu sync.RWMutex
}

func newClashServer(ctx context.Context, _ log.ObservableFactory, options O.ClashAPIOptions) (A.ClashServer, error) {
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

	runCtx, cancel := context.WithCancel(ctx)
	ct := newConnTracker()
	tp := newThroughputTracker(ct, 0)
	ct.tp = tp
	return &clashServer{
		ctx:               runCtx,
		cancel:            cancel,
		dnsRouter:         service.FromContext[A.DNSRouter](ctx),
		outbound:          service.FromContext[A.OutboundManager](ctx),
		endpoint:          service.FromContext[A.EndpointManager](ctx),
		connTracker:       ct,
		throughputTracker: tp,
		modeList:          modeList,
		mode:              initial,
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
	if s.rejecting.Load() {
		return rejectMode
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

func (s *clashServer) ModeList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.modeList
}

func (s *clashServer) Start(stage A.StartStage) error {
	s.startOnce.Do(func() {
		s.trackerDone = make(chan struct{})
		go func() {
			defer close(s.trackerDone)
			s.throughputTracker.Run(s.ctx)
		}()
	})
	return nil
}

func (s *clashServer) Close() error {
	s.cancel()
	if s.trackerDone != nil {
		<-s.trackerDone
	}
	return nil
}

func (s *clashServer) SetAdmissionGate(gate dialAdmissionGate) {
	if gate != nil {
		s.admissionGate.Store(gate)
	}
}

func (s *clashServer) setRejectMode(enable bool) {
	if swapped := s.rejecting.Swap(enable); swapped == enable {
		return
	}
	if enable {
		slog.Warn("Enabling reject mode: all connections will be rejected until disabled")
		return
	}
	slog.Info("Disabling reject mode: routing will resume as normal")
}

// HistoryStorage always returns nil. [A.URLTestHistoryStorage] is not used by [clashServer]
// so this is to satisfy [A.ClashServer].
func (s *clashServer) HistoryStorage() A.URLTestHistoryStorage {
	return nil
}

func (s *clashServer) ThroughputTracker() *throughputTracker {
	return s.throughputTracker
}

// newRecord builds a lean record for a routed connection, copying the scalars radiance reads out of
// metadata and resolving the outbound chain.
func (s *clashServer) newRecord(metadata A.InboundContext, matchedRule A.Rule, matchOutbound A.Outbound) *record {
	id, _ := uuid.NewV4()
	outbound, outboundType, chain := s.resolveChain(matchOutbound)
	return &record{
		id:           id,
		createdAt:    time.Now(),
		outbound:     outbound,
		outboundType: outboundType,
		chain:        chain,
		inboundType:  metadata.InboundType,
		inboundName:  metadata.Inbound,
		ipVersion:    metadata.IPVersion,
		network:      metadata.Network,
		source:       metadata.Source.String(),
		destination:  metadata.Destination.String(),
		domain:       metadata.Domain,
		protocol:     metadata.Protocol,
		fromOutbound: metadata.Outbound,
		ruleStr:      formatRule(matchedRule),
	}
}

func (s *clashServer) resolveChain(matchOutbound A.Outbound) (outbound, outboundType string, chain []string) {
	var next string
	if matchOutbound != nil {
		next = matchOutbound.Tag()
	} else {
		next = s.outbound.Default().Tag()
	}

	for next != "" {
		detour, loaded := s.outbound.Outbound(next)
		if !loaded {
			if outbound == "" {
				outbound = next
			}
			break
		}

		chain = append(chain, next)
		outbound = detour.Tag()
		outboundType = detour.Type()

		group, isGroup := detour.(A.OutboundGroup)
		if !isGroup {
			break
		}
		next = group.Now()
	}

	return outbound, outboundType, common.Reverse(chain)
}

func formatRule(rule A.Rule) string {
	if rule == nil {
		return ""
	}
	return rule.String() + " => " + rule.Action().String()
}

func (s *clashServer) uploadCounter(r *record) N.CountFunc {
	return func(n int64) {
		r.upload.Add(n)
		s.connTracker.pushUploaded(n)
	}
}

func (s *clashServer) downloadCounter(r *record) N.CountFunc {
	return func(n int64) {
		r.download.Add(n)
		s.connTracker.pushDownloaded(n)
	}
}

func (s *clashServer) admitConnection(now time.Time) {
	if gate, _ := s.admissionGate.Load().(dialAdmissionGate); gate != nil {
		gate.RecordDial(now)
	}
}

func (s *clashServer) RoutedConnection(ctx context.Context, conn net.Conn, metadata A.InboundContext, matchedRule A.Rule, matchOutbound A.Outbound) net.Conn {
	s.admitConnection(time.Now())

	r := s.newRecord(metadata, matchedRule, matchOutbound)
	c := &tcpConn{
		ExtendedConn: bufio.NewCounterConn(
			conn,
			[]N.CountFunc{s.uploadCounter(r)},
			[]N.CountFunc{s.downloadCounter(r)},
		),
		rec: r,
		ct:  s.connTracker,
	}
	r.closer = c
	s.connTracker.join(r)
	return c
}

func (s *clashServer) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata A.InboundContext, matchedRule A.Rule, matchOutbound A.Outbound) N.PacketConn {
	s.admitConnection(time.Now())

	r := s.newRecord(metadata, matchedRule, matchOutbound)
	c := &udpConn{
		PacketConn: bufio.NewCounterPacketConn(
			conn,
			[]N.CountFunc{s.uploadCounter(r)},
			[]N.CountFunc{s.downloadCounter(r)},
		),
		rec: r,
		ct:  s.connTracker,
	}
	r.closer = c
	s.connTracker.join(r)
	return c
}

func (s *clashServer) Name() string {
	return "clash"
}
