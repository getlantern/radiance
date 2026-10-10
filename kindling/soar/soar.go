// Package soar builds the Soar DNS-tunnel transport for kindling: the last-resort path that
// still works when only recursive DNS does.
package soar

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/getlantern/soar"

	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/settings"
)

// Zone and PublicKey identify the Soar server. They're set at build time
// (-ldflags "-X github.com/getlantern/radiance/kindling/soar.Zone=... -X ...PublicKey=...")
// rather than committed: a public repo naming the zone hands a censor the string to block, and
// the tunnel is a bootstrap path, so it can't wait for fetched config.
var (
	Zone      string
	PublicKey string
)

// Configured reports whether this build carries a Soar server.
func Configured() bool { return Zone != "" && PublicKey != "" }

// New returns a Soar client: resolvers come from discovery (the bundled per-country lists for
// the user's country, the device's own resolvers, and a per-device cache of ones that worked),
// and TCP/53 goes through radiance's bypass dialer so it never loops through the VPN.
func New(dataDir string) (*soar.Client, error) {
	if !Configured() {
		return nil, errors.New("soar: no server configured in this build")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(PublicKey))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("soar: bad public key")
	}
	return soar.NewClient(soar.Config{
		Zone:      Zone,
		ServerKey: ed25519.PublicKey(key),
		DialTCP:   bypass.DialContext,
		Discovery: soar.Discovery{
			Enabled: true,
			Country: settings.GetString(settings.CountryCodeKey),
			Cache:   &fileCache{path: filepath.Join(dataDir, "soar_resolvers.json")},
		},
		Logger: slog.Default().With("component", "soar"),
	})
}

// fileCache persists resolvers that carried the tunnel, keyed by country, so the next start
// vets them first instead of rediscovering.
type fileCache struct {
	mu   sync.Mutex
	path string
}

func (f *fileCache) key() string {
	if cc := settings.GetString(settings.CountryCodeKey); cc != "" {
		return strings.ToUpper(cc)
	}
	return "_"
}

func (f *fileCache) load() map[string][]string {
	m := map[string][]string{}
	if b, err := os.ReadFile(f.path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (f *fileCache) Load() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.load()[f.key()]
}

func (f *fileCache) Store(resolvers []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.load()
	m[f.key()] = resolvers
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := atomicfile.WriteFile(f.path, b, 0o600); err != nil {
		slog.Debug("soar: storing resolver cache", "error", err)
	}
}
