package kindling

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/kindling/dnstt"
	"github.com/getlantern/radiance/kindling/fronted"
)

var k kindling.Kindling = NewKindling()

// NewKindling builds a kindling client and bootstrap this package
func NewKindling(options ...kindling.Option) kindling.Kindling {
	dataDir := settings.GetString(settings.DataPathKey)
	f, err := fronted.NewFronted(reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"), &slogWriter{Logger: slog.Default()})
	if err != nil {
		return &mockKindling{}
	}

	ampClient, err := fronted.NewAMPClient(context.Background(), &slogWriter{Logger: slog.Default()})
	if err != nil {
		return &mockKindling{}
	}

	opts := []kindling.Option{
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithLogWriter(&slogWriter{Logger: slog.Default()}),
		kindling.WithDomainFronting(f),
		// Most endpoints use df.iantem.io, but for some historical reasons
		// "pro-server" calls still go to api.getiantem.org.
		kindling.WithProxyless("df.iantem.io", "api.getiantem.org"),
		// Kindling will skip amp transports if the request has a payload larger than 6kb
		kindling.WithAMPCache(ampClient),
	}
	opts = append(opts, options...)
	return kindling.NewKindling("radiance", opts...)
}

// HTTPClient returns a http client with kindling transport
func HTTPClient() *http.Client {
	return k.NewHTTPClient()
}

// SetKindling sets the kindling method used for building the HTTP client
// This function is useful for testing purposes.
func SetKindling(a kindling.Kindling) {
	k = a
}

// KindlingUpdater start event subscriptions that might need to rebuild kindling
func KindlingUpdater() {
	events.Subscribe(func(e dnstt.DNSTTUpdateEvent) {
		options, err := dnstt.ParseDNSTTConfigs(e.YML)
		if err != nil {
			slog.Warn("could not update dnstt options", slog.Any("error", err))
			return
		}
		// replace dnstt renewable options once there's new options available
		SetKindling(NewKindling(options...))
	})
}

type mockKindling struct {
	kindling.Kindling
}

// Make sure mockKindling implements kindling.Kindling
var _ kindling.Kindling = (*mockKindling)(nil)

func (m *mockKindling) NewHTTPClient() *http.Client {
	return &http.Client{}
}

func (m *mockKindling) ReplaceTransport(name string, rt func(ctx context.Context, addr string) (http.RoundTripper, error)) error {
	return nil
}

type slogWriter struct {
	*slog.Logger
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	// Convert the byte slice to a string and log it
	w.Info(string(p))
	return len(p), nil
}
