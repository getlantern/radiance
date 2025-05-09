package radiance

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/Xuanwo/go-locale"
	"github.com/getlantern/appdir"
	"github.com/getlantern/fronted"
	"github.com/getlantern/kindling"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/client"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/deviceid"
	"github.com/getlantern/radiance/common/reporting"
)

var (
	sharedInitOnce sync.Once
	sharedInit     *sharedConfig
)

// sharedConfig is a struct that contains the shared configuration for the Radiance client and API handler.
type sharedConfig struct {
	logWriter io.Writer
	userInfo  common.UserInfo
	kindling  kindling.Kindling
}

// initCommon initializes the common configuration for the Radiance client and API handler.
func initialize(opts client.Options) (*sharedConfig, error) {
	var err error
	sharedInitOnce.Do(func() {
		reporting.Init()
		if opts.DataDir == "" {
			opts.DataDir = appdir.General(app.Name)
		}
		if opts.LogDir == "" {
			opts.LogDir = appdir.Logs(app.Name)
		}
		if opts.Locale == "" {
			// It is preferable to use the locale from the frontend, as locale is a requirement for lots
			// of frontend code and therefore is more reliably supported there.
			// However, if the frontend locale is not available, we can use the system locale as a fallback.
			if tag, err := locale.Detect(); err != nil {
				opts.Locale = "en-US"
			} else {
				opts.Locale = tag.String()
			}
		}

		var platformDeviceID string
		if common.IsAndroid() || common.IsIOS() {
			platformDeviceID = opts.DeviceID
		} else {
			platformDeviceID = deviceid.Get()
		}

		mkdirs(&opts)
		var logWriter io.Writer
		logWriter, err = newLog(filepath.Join(opts.LogDir, app.LogFileName))
		if err != nil {
			err = fmt.Errorf("could not create log: %w", err)
			return
		}

		f, ferr := newFronted(logWriter, reporting.PanicListener, filepath.Join(opts.DataDir, "fronted_cache.json"))
		if ferr != nil {
			err = fmt.Errorf("failed to create fronted: %w", ferr)
			return
		}

		k := kindling.NewKindling(
			kindling.WithPanicListener(reporting.PanicListener),
			kindling.WithLogWriter(logWriter),
			kindling.WithDomainFronting(f),
			kindling.WithProxyless("api.iantem.io"))

		sharedInit = &sharedConfig{
			logWriter: logWriter,
			userInfo:  common.NewUserConfig(platformDeviceID, opts.DataDir, opts.Locale),
			kindling:  k,
		}
		slog.Debug("Initialized shared config", "dataDir", opts.DataDir, "logDir", opts.LogDir, "locale", opts.Locale)
	})

	if err != nil {
		slog.Error("Failed to initialize shared config", "error", err)
	}
	return sharedInit, err

}

func mkdirs(opts *client.Options) {
	// Make sure the data and logs dirs exist
	for _, dir := range []string{opts.DataDir, opts.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Error("Failed to create data directory", "dir", dir, "error", err)
		}
	}
}

// Return an slog logger configured to write to both stdout and the log file.
func newLog(logPath string) (io.Writer, error) {
	// If the log file does not exist, create it.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}
	// defer f.Close() - file should be closed externally when logger is no longer needed
	logWriter := io.MultiWriter(os.Stdout, f)
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{
		AddSource: true,
	}))
	slog.SetDefault(logger)
	return logWriter, nil
}

func newFronted(logWriter io.Writer, panicListener func(string), cacheFile string) (fronted.Fronted, error) {
	// Parse the domain from the URL.
	configURL := "https://raw.githubusercontent.com/getlantern/lantern-binaries/refs/heads/main/fronted.yaml.gz"
	u, err := url.Parse(configURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %v", err)
	}
	// Extract the domain from the URL.
	domain := u.Host

	// First, download the file from the specified URL using the smart dialer.
	// Then, create a new fronted instance with the downloaded file.
	trans, err := kindling.NewSmartHTTPTransport(logWriter, domain)
	if err != nil {
		// This will happen, for example, when we're offline.
		slog.Info("failed to create smart HTTP transport. offline?", "error", err)
	}
	lz := &lazyDialingRoundTripper{
		smartTransportMu: sync.Mutex{},
		logWriter:        logWriter,
		domain:           domain,
	}
	if trans != nil {
		lz.smartTransport = trans
	}

	httpClient := &http.Client{
		Transport: lz,
	}
	return fronted.NewFronted(
		fronted.WithPanicListener(panicListener),
		fronted.WithCacheFile(cacheFile),
		fronted.WithHTTPClient(httpClient),
		fronted.WithConfigURL(configURL),
	), nil
}

// This is a lazy RoundTripper that allows radiance to start without an error
// when it's offline.
type lazyDialingRoundTripper struct {
	smartTransport   http.RoundTripper
	smartTransportMu sync.Mutex

	logWriter io.Writer
	domain    string
}

// Make sure lazyDialingRoundTripper implements http.RoundTripper
var _ http.RoundTripper = (*lazyDialingRoundTripper)(nil)

func (lz *lazyDialingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	lz.smartTransportMu.Lock()

	if lz.smartTransport == nil {
		slog.Info("Smart transport is nil")
		var err error
		lz.smartTransport, err = kindling.NewSmartHTTPTransport(lz.logWriter, lz.domain)
		if err != nil {
			slog.Info("Error creating smart transport", "error", err)
			lz.smartTransportMu.Unlock()
			// This typically just means we're offline
			return nil, fmt.Errorf("could not create smart transport -- offline? %v", err)
		}
	}

	lz.smartTransportMu.Unlock()
	return lz.smartTransport.RoundTrip(req)
}
