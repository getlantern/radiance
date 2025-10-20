package radiance

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/getlantern/radiance/vpn"
)

// AdBlockOptions controls the built-in ad blocker
type AdBlockOptions struct {
	Enabled bool
	// one or more rule-set URLs (v3 RuleSet JSON)
	Sources      []string
	RefreshEvery time.Duration
}

// adblockHost contains the running ad block handler so we can toggle/refresh it
type adblockHost struct {
	impl *vpn.AdBlocker
}

func (h *adblockHost) setEnabled(v bool) error {
	if h.impl == nil {
		return fmt.Errorf("ad blocker is not configured")
	}
	return h.impl.SetEnabled(v)
}

func (h *adblockHost) isEnabled() bool {
	if h.impl == nil {
		return false
	}
	return h.impl.IsEnabled()
}

func (h *adblockHost) refresh(ctx context.Context) error {
	if h.impl == nil {
		return fmt.Errorf("ad blocker is not configured")
	}
	return h.impl.Refresh(ctx)
}

// newAdBlockHost initializes the ad blocker with the usable sources provided
func newAdBlockHost(
	ctx context.Context,
	client *http.Client,
	opts *AdBlockOptions,
) (*adblockHost, func(context.Context) error, error) {
	if opts == nil {
		return &adblockHost{}, func(context.Context) error { return nil }, nil
	}
	var srcPick string
	for _, u := range opts.Sources {
		if u != "" {
			srcPick = u
			break
		}
	}
	if srcPick == "" {
		return &adblockHost{}, func(context.Context) error { return nil }, nil
	}

	ab, err := vpn.NewAdBlockerHandler(client, srcPick, opts.RefreshEvery)
	if err != nil {
		return nil, nil, fmt.Errorf("create ad blocker: %w", err)
	}
	if err := ab.SetEnabled(opts.Enabled); err != nil {
		return nil, nil, fmt.Errorf("set ad blocker enabled: %w", err)
	}
	ab.Start(ctx)

	cleanup := func(_ context.Context) error {
		ab.Stop()
		return nil
	}
	return &adblockHost{impl: ab}, cleanup, nil
}
