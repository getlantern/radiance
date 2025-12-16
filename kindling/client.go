package kindling

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/kindling/dnstt"
	"github.com/getlantern/radiance/kindling/fronted"
	"github.com/getlantern/radiance/traces"
)

var (
	httpClient *http.Client
	// defaultOptions generally does not change after the first time
	// or if they change, it's handled internally
	defaultOptions = make([]kindling.Option, 0)
	// dnsttRenewableOptions is a list that is overwritten whenever we receive
	// a new dnstt config and we use that for rebuilding kindling
	dnsttRenewableOptions = make([]kindling.Option, 0)
	mutexOptions          sync.Mutex
)

// HTTPClient returns a http client with kindling transport
func HTTPClient() *http.Client {
	mutexOptions.Lock()
	defer mutexOptions.Unlock()
	return httpClient
}

func newHTTPClient(k kindling.Kindling) {
	httpClient = k.NewHTTPClient()
	httpClient.Timeout = common.DefaultHTTPTimeout
	httpClient.Transport = traces.NewRoundTripper(traces.NewHeaderAnnotatingRoundTripper(httpClient.Transport))
}

// NewKindling build a kindling client and bootstrap this package
func NewKindling(dataDir string, logger io.Writer) error {
	mutexOptions.Lock()
	defer mutexOptions.Unlock()

	if len(defaultOptions) == 0 {
		f, err := fronted.NewFronted(reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"), logger)
		if err != nil {
			return fmt.Errorf("failed to create fronted: %w", err)
		}
		defaultOptions = append(defaultOptions,
			kindling.WithPanicListener(reporting.PanicListener),
			kindling.WithLogWriter(logger),
			kindling.WithDomainFronting(f),
			// Most endpoints use df.iantem.io, but for some historical reasons
			// "pro-server" calls still go to api.getiantem.org.
			kindling.WithProxyless("df.iantem.io", "api.getiantem.org"),
		)
	}

	newHTTPClient(kindling.NewKindling("radiance", defaultOptions...))
	return nil
}

type ClientUpdated struct {
	events.Event
}

// KindlingUpdater start event subscriptions that might need to rebuild kindling
func KindlingUpdater() {
	events.Subscribe(func(e dnstt.DNSTTUpdateEvent) {
		mutexOptions.Lock()
		defer mutexOptions.Unlock()

		options, err := dnstt.ParseDNSTTConfigs(e.YML)
		if err != nil {
			slog.Warn("could not update dnstt options", slog.Any("error", err))
			return
		}
		// replace dnstt renewable options once there's new options available
		dnsttRenewableOptions = options

		// build new http client
		newHTTPClient(kindling.NewKindling("radiance", append(defaultOptions, dnsttRenewableOptions...)...))
		// notify that a new client is available
		events.Emit(ClientUpdated{})
	})
}
