package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"reflect"
	"sync"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/json"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/helper"
	"github.com/getlantern/radiance/internal/hostaccess"
)

// bridgeLogName names every bridge's logger. It leaves out the tag, a
// pseudonym of the pairing, and its "helper" makes the log writer redact
// addresses on the bridge's lines.
const bridgeLogName = "inbound/helper[" + lbC.TypeSamizdatBridge + "]"

// NewBridgeBox builds a stopped sing-box instance, outside the VPN tunnel, for
// a helper's bridges, from lantern-cloud's bridge config. It refuses a config
// without direct mode's abuse rules (validateAbuseRules). Its dials bind to
// the physical interface, it logs only warnings and errors, and it calls count
// with a bridge's tag for the bytes each of its connections relays.
func NewBridgeBox(ctx context.Context, bridgeConfig string, count func(tag string, n int64)) (helper.BridgeBox, error) {
	// The direct outbound logs each destination at info.
	ctx = newBoxContextLoggingTo(ctx, minLevelHandler{Handler: slog.Default().Handler(), min: slog.LevelWarn})
	opts, err := bridgeBoxOptions(ctx, bridgeConfig)
	if err != nil {
		return nil, err
	}
	bs, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	if err != nil {
		return nil, fmt.Errorf("build bridge box: %w", err)
	}
	bs.Router().AppendTracker(bridgeTracker{count: count})
	return &bridgeBox{
		ctx:     ctx,
		box:     bs,
		logger:  service.FromContext[sblog.Factory](ctx).NewLogger(bridgeLogName),
		bridges: make(map[string]lbO.SamizdatBridgeInboundOptions),
	}, nil
}

func bridgeBoxOptions(ctx context.Context, bridgeConfig string) (option.Options, error) {
	// A bridge exits like a direct-mode peer, under the same abuse rules.
	if err := validateAbuseRules(bridgeConfig); err != nil {
		return option.Options{}, fmt.Errorf("bridge config: %w", err)
	}
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(bridgeConfig))
	if err != nil {
		return option.Options{}, fmt.Errorf("decode bridge config: %w", err)
	}
	// Bridges are the box's only inbounds, and nothing else may listen:
	// services and endpoints can bind listeners too.
	opts.Inbounds, opts.Endpoints, opts.Services, opts.Experimental = nil, nil, nil, nil
	// The registry checks don't see route rule actions.
	if err := hostaccess.CheckOptions(opts); err != nil {
		return option.Options{}, fmt.Errorf("bridge config: %w", err)
	}
	// A bridge's traffic must leave from the helper's own connection, not
	// through the VPN tunnel.
	opts.Route.AutoDetectInterface = true
	return opts, nil
}

type bridgeBox struct {
	ctx    context.Context
	box    *sbox.Box
	logger sblog.ContextLogger

	mu      sync.Mutex
	bridges map[string]lbO.SamizdatBridgeInboundOptions
}

func (b *bridgeBox) Start() error { return b.box.Start() }

func (b *bridgeBox) Close() error { return b.box.Close() }

// SetBridges creates, replaces and removes inbounds so the box runs exactly
// bridges. A bridge that fails to be created keeps its predecessor, if any,
// and the next call retries it. Errors name tags.
func (b *bridgeBox) SetBridges(bridges map[string]*lbO.SamizdatBridgeInboundOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	inbounds := b.box.Inbound()
	var errs []error
	for tag := range b.bridges {
		if _, ok := bridges[tag]; ok {
			continue
		}
		if err := inbounds.Remove(tag); err != nil && !errors.Is(err, os.ErrInvalid) {
			errs = append(errs, fmt.Errorf("remove bridge %s: %w", tag, err))
		}
		delete(b.bridges, tag)
	}
	for tag, opts := range bridges {
		if opts == nil {
			continue
		}
		if cur, ok := b.bridges[tag]; ok && reflect.DeepEqual(cur, *opts) {
			continue
		}
		o := *opts
		o.Addrs = append([]string(nil), opts.Addrs...)
		// Create closes the bridge it replaces.
		if err := inbounds.Create(b.ctx, b.box.Router(), b.logger, tag, lbC.TypeSamizdatBridge, &o); err != nil {
			errs = append(errs, fmt.Errorf("create bridge %s: %w", tag, err))
			continue
		}
		b.bridges[tag] = o
	}
	return errors.Join(errs...)
}

// bridgeTracker counts the bytes of every connection the bridge box routes
// against the inbound that accepted it.
type bridgeTracker struct {
	count func(tag string, n int64)
}

func (t bridgeTracker) RoutedConnection(_ context.Context, conn net.Conn, metadata adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) net.Conn {
	counter := t.counter(metadata.Inbound)
	return bufio.NewCounterConn(conn, counter, counter)
}

func (t bridgeTracker) RoutedPacketConnection(_ context.Context, conn N.PacketConn, metadata adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) N.PacketConn {
	counter := t.counter(metadata.Inbound)
	return bufio.NewCounterPacketConn(conn, counter, counter)
}

// RoutedFlow returns nil: a bridge box has no TUN inbound, so it routes no flows.
func (t bridgeTracker) RoutedFlow(context.Context, adapter.InboundContext, adapter.Rule, adapter.Outbound) tun.FlowTracker {
	return nil
}

func (t bridgeTracker) counter(tag string) []N.CountFunc {
	return []N.CountFunc{func(n int64) { t.count(tag, n) }}
}

// minLevelHandler drops records below min.
type minLevelHandler struct {
	slog.Handler
	min slog.Level
}

func (h minLevelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.min && h.Handler.Enabled(ctx, level)
}

func (h minLevelHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < h.min {
		return nil
	}
	return h.Handler.Handle(ctx, r)
}

func (h minLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevelHandler{Handler: h.Handler.WithAttrs(attrs), min: h.min}
}

func (h minLevelHandler) WithGroup(name string) slog.Handler {
	return minLevelHandler{Handler: h.Handler.WithGroup(name), min: h.min}
}
