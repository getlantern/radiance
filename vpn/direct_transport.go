package vpn

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

// lazyDirectTransport is an http.RoundTripper registered in the sing-box
// context before the service is created. It defers to an inner RoundTripper
// that is set via Resolve() after the direct outbound exists.
//
// This allows unbounded (which is constructed during NewServiceWithContext)
// to hold a reference to this transport, while the actual dialer is wired
// later — before PostStart, when unbounded first uses it.
type lazyDirectTransport struct {
	mu       sync.RWMutex
	inner    http.RoundTripper
	resolved bool
}

func (t *lazyDirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.resolved || t.inner == nil {
		return nil, fmt.Errorf("direct transport not yet resolved")
	}
	return t.inner.RoundTrip(req)
}

// Resolve builds the underlying http.Transport using the direct outbound's
// DialContext. Must be called after libbox.NewServiceWithContext but before
// the sing-box service Start (so that it's ready by PostStart when unbounded
// first makes HTTP calls).
func (t *lazyDirectTransport) Resolve(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	outboundMgr := service.FromContext[adapter.OutboundManager](ctx)
	if outboundMgr == nil {
		return fmt.Errorf("outbound manager not found in context")
	}

	directOutbound, found := outboundMgr.Outbound("direct")
	if !found {
		return fmt.Errorf("direct outbound not found")
	}

	dialer, ok := directOutbound.(N.Dialer)
	if !ok {
		return fmt.Errorf("direct outbound does not implement N.Dialer")
	}

	t.inner = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, M.ParseSocksaddr(addr))
		},
	}
	t.resolved = true
	slog.Debug("Direct transport resolved using sing-box direct outbound")
	return nil
}
