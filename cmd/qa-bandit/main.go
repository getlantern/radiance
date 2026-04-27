// Command qa-bandit is a focused QA driver for the bandit assignment path.
// It boots a radiance backend that impersonates an Android client, captures
// the first /v1/config-new response from the bandit, prints the assignment,
// then optionally connects the VPN and probes a target URL through the
// resulting tunnel to confirm both the API view of the client and the
// outbound dials originate from the country we're simulating.
//
// Pair with `pinger bridge --country ru`:
//
//	# in lantern-cloud-bridge:
//	./cmd/pinger/bridge.sh
//	# in radiance:
//	RADIANCE_OUTBOUND_SOCKS_ADDRESS=127.0.0.1:1080 \
//	  go run -tags 'with_quic,with_gvisor,with_wireguard,with_utls' ./cmd/qa-bandit
//
// The build tags are needed by sing-box outbounds (hysteria2 needs QUIC,
// etc.) — without them ConnectVPN fails with "X is not included in this
// build, rebuild with -tags with_X".
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"golang.org/x/net/proxy"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/vpn"
)

func main() {
	var (
		outboundSocks = flag.String("outbound-socks", os.Getenv("RADIANCE_OUTBOUND_SOCKS_ADDRESS"),
			"upstream SOCKS5 to route ALL radiance egress through (e.g. 127.0.0.1:1080 — pinger bridge)")
		platform = flag.String("platform", "android", "platform to advertise to the API (sent in the body and X-Lantern-Platform)")
		version  = flag.String("version", "9.0.30-qa-bandit",
			"app version to advertise (X-Lantern-App-Version / X-Lantern-Version)")
		deviceID  = flag.String("device-id", "qa-bandit-android-0001", "device ID to advertise")
		userID    = flag.String("user-id", "0", "user ID to advertise (string; 0 = no specific user)")
		token     = flag.String("token", "", "pro token (optional — empty = free tier)")
		probeURL  = flag.String("probe-url", "https://api.ipify.org",
			"URL to fetch through the bandit-assigned tunnel to verify egress IP")
		doConnect = flag.Bool("connect", true,
			"actually ConnectVPN(AutoSelect) and probe — false = just dump the bandit response and exit")
		socksIn = flag.String("socks-inbound", "127.0.0.1:46666",
			"local SOCKS5 inbound that radiance exposes for the probe (avoids needing a TUN / root)")
		// The API's GeoIP→country logic overrides the IP-derived country
		// with the timezone-derived one (treats mismatches as VPN users).
		// Without spoofing these to Russia equivalents, the bandit will
		// keep serving US-tier outbounds even though the TCP egress is
		// Russia. See cmd/api/maxmind.go LookupCountryASNState.
		tz      = flag.String("tz", "Europe/Moscow", "TZ env var sent as the request's X-Lantern-Time-Zone")
		locale  = flag.String("locale", "ru_RU", "locale to pass to the radiance backend (X-Lantern-Locale)")
		timeout = flag.Duration("timeout", 180*time.Second, "overall timeout (covers config fetch + URLTest convergence + probe retries)")
	)
	flag.Parse()

	// Plumb the QA env vars BEFORE common.Init runs (i.e. before NewLocalBackend).
	// All three of these are honored by code on qa/outbound-socks-egress branch.
	if *outboundSocks != "" {
		os.Setenv("RADIANCE_OUTBOUND_SOCKS_ADDRESS", *outboundSocks)
	}
	os.Setenv("RADIANCE_PLATFORM", *platform)
	os.Setenv("RADIANCE_VERSION", *version)
	// Use a SOCKS5 inbound listener instead of a TUN device — no root/sudo
	// needed, and gives us a clean address to probe through.
	os.Setenv("RADIANCE_USE_SOCKS_PROXY", "true")
	os.Setenv("RADIANCE_SOCKS_ADDRESS", *socksIn)
	// Spoof TZ so the X-Lantern-Time-Zone radiance sends matches the country
	// we're impersonating. The API's MaxMind logic overrides the GeoIP-derived
	// country with the timezone-derived one when they disagree, so without
	// this the bandit thinks "user behind a VPN, return their real country".
	if *tz != "" {
		os.Setenv("TZ", *tz)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	dataDir, err := os.MkdirTemp("", "qa-bandit-")
	if err != nil {
		fatal("mktempdir", err)
	}
	defer os.RemoveAll(dataDir)

	banner(*outboundSocks, *platform, *version, dataDir, *socksIn)

	be, err := backend.NewLocalBackend(ctx, backend.Options{
		DataDir: dataDir,
		LogDir:  dataDir,
		Locale:  *locale,
	})
	if err != nil {
		fatal("NewLocalBackend", err)
	}
	defer be.Close()

	uid, err := strconv.ParseInt(*userID, 10, 64)
	if err != nil {
		fatal("parse user-id", err)
	}
	settings.Set(settings.UserIDKey, uid)
	settings.Set(settings.TokenKey, *token)
	settings.Set(settings.UserLevelKey, "")
	settings.Set(settings.EmailKey, "qa-bandit@local")
	// Need both: DeviceIDKey is what common.NewRequestWithHeaders pulls for
	// the X-Lantern-DeviceID header (and the user-create body field), while
	// DevicesKey is the canonical list used elsewhere.
	settings.Set(settings.DeviceIDKey, *deviceID)
	settings.Set(settings.DevicesKey, []settings.Device{{ID: *deviceID, Name: *deviceID}})

	// Subscribe BEFORE Start() so we don't race the first config event.
	cfgCh := make(chan *config.Config, 1)
	go events.SubscribeOnce(func(evt config.NewConfigEvent) {
		cfgCh <- evt.New
	})

	be.Start()

	// Note: we deliberately do NOT bring up the IPC server here. It's there
	// for client UIs (Lantern Flutter, etc.) to talk to the backend — we're
	// calling backend methods directly, and on macOS its default Unix-socket
	// path (/var/run/lantern/lanternd.sock) requires root.

	fmt.Println("[qa-bandit] waiting for first /v1/config-new response (bandit assignment)...")
	var cfg *config.Config
	select {
	case cfg = <-cfgCh:
	case <-ctx.Done():
		fatal("waiting for config", ctx.Err())
	}

	dumpAssignment(cfg)

	if !*doConnect {
		return
	}

	fmt.Println("\n[qa-bandit] connecting VPN with bandit auto-pick...")
	if err := be.ConnectVPN(vpn.AutoSelectTag); err != nil {
		fmt.Printf("[qa-bandit] ConnectVPN FAILED: %v\n", err)
		os.Exit(1)
	}
	defer be.DisconnectVPN()

	// URLTest needs a few seconds to converge on a working outbound. UDP
	// outbounds (hysteria2/wireguard/tuic) fail immediately through our
	// bridge — it only does TCP CONNECT. After URLTest marks them dead,
	// AutoSelect prefers TCP-based ones (samizdat/reflex/vmess/etc.).
	fmt.Printf("[qa-bandit] VPN connected; waiting up to 30s for URLTest to converge, then probing %s through %s...\n", *probeURL, *socksIn)
	var (
		body string
		dur  time.Duration
		err2 error
	)
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 1; ; attempt++ {
		body, dur, err2 = probeViaSocks(ctx, *socksIn, *probeURL)
		if err2 == nil {
			fmt.Printf("[qa-bandit] probe OK in %.2fs (attempt %d) — egress IP: %s\n", dur.Seconds(), attempt, body)
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			fmt.Printf("[qa-bandit] probe FAILED after %d attempts: %v\n", attempt, err2)
			os.Exit(1)
		}
		fmt.Printf("[qa-bandit]   attempt %d failed (%v) — retrying in 3s...\n", attempt, err2)
		time.Sleep(3 * time.Second)
	}
}

func banner(outboundSocks, platform, version, dataDir, socksIn string) {
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println(" qa-bandit — radiance bandit-assignment probe")
	fmt.Println("======================================================================")
	fmt.Printf("  Platform           : %s\n", platform)
	fmt.Printf("  App version        : %s\n", version)
	fmt.Printf("  Time zone          : %s\n", os.Getenv("TZ"))
	if outboundSocks == "" {
		fmt.Println("  Outbound SOCKS5    : (unset — radiance will dial DIRECTLY, NOT through any country)")
	} else {
		fmt.Printf("  Outbound SOCKS5    : %s (every radiance dial goes here)\n", outboundSocks)
	}
	fmt.Printf("  Probe inbound SOCKS: %s\n", socksIn)
	fmt.Printf("  Data dir           : %s\n", dataDir)
	fmt.Println()
}

// dumpAssignment prints the parts of the config response the bandit decided.
func dumpAssignment(cfg *config.Config) {
	fmt.Println("=========================== bandit assignment ===========================")
	fmt.Printf("  API saw client as     : country=%s ip=%s\n", cfg.Country, cfg.IP)
	fmt.Printf("  Servers (%d) :\n", len(cfg.Servers))
	for _, s := range cfg.Servers {
		fmt.Printf("      %-2s %s / %s\n", s.CountryCode, s.Country, s.City)
	}
	fmt.Printf("  Outbounds (%d):\n", len(cfg.Options.Outbounds))
	for _, o := range cfg.Options.Outbounds {
		loc := cfg.OutboundLocations[o.Tag]
		fmt.Printf("      %-12s %s  (%s / %s)\n", o.Type, o.Tag, loc.CountryCode, loc.City)
	}
	if len(cfg.BanditURLOverrides) > 0 {
		fmt.Printf("  Bandit callback URLs : %d outbounds tagged with per-proxy callbacks\n", len(cfg.BanditURLOverrides))
	}
	if cfg.PollIntervalSeconds > 0 {
		fmt.Printf("  Server-suggested poll: %ds\n", cfg.PollIntervalSeconds)
	}
	if raw, err := json.MarshalIndent(struct {
		Country             string            `json:"country"`
		IP                  string            `json:"ip"`
		Outbounds           int               `json:"outbounds"`
		Servers             int               `json:"servers"`
		BanditURLOverrides  int               `json:"bandit_url_overrides"`
		PollIntervalSeconds int               `json:"poll_interval_seconds"`
		OutboundLocations   map[string]string `json:"outbound_locations,omitempty"`
	}{
		Country:             cfg.Country,
		IP:                  cfg.IP,
		Outbounds:           len(cfg.Options.Outbounds),
		Servers:             len(cfg.Servers),
		BanditURLOverrides:  len(cfg.BanditURLOverrides),
		PollIntervalSeconds: cfg.PollIntervalSeconds,
		OutboundLocations:   shortOutboundLocations(cfg),
	}, "", "  "); err == nil {
		fmt.Printf("  Summary JSON         :\n%s\n", raw)
	}
	fmt.Println("==========================================================================")
}

func shortOutboundLocations(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.OutboundLocations))
	for tag, loc := range cfg.OutboundLocations {
		out[tag] = fmt.Sprintf("%s / %s", loc.CountryCode, loc.City)
	}
	return out
}

func probeViaSocks(ctx context.Context, socksAddr, target string) (string, time.Duration, error) {
	d, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		return "", 0, fmt.Errorf("building SOCKS5 dialer to %s: %w", socksAddr, err)
	}
	cd := d.(proxy.ContextDialer)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return cd.DialContext(ctx, network, addr)
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	parsed, err := url.Parse(target)
	if err != nil {
		return "", 0, fmt.Errorf("parsing %q: %w", target, err)
	}
	if parsed.Scheme == "" {
		return "", 0, fmt.Errorf("probe URL must include scheme (https://...): %q", target)
	}

	t0 := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Since(t0), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", time.Since(t0), fmt.Errorf("probe returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", time.Since(t0), err
	}
	return string(body), time.Since(t0), nil
}

func fatal(stage string, err error) {
	slog.Error(stage, "error", err)
	fmt.Fprintf(os.Stderr, "[qa-bandit] FAILED at %s: %v\n", stage, err)
	os.Exit(1)
}

// Compile-time check that common.Platform is a var (not const) — see
// common/platform.go. If this stops compiling, the override env var
// (RADIANCE_PLATFORM, set in main()) won't take effect.
var _ = func() string { return common.Platform }
