// Package settings provides a simple interface for storing and retrieving user settings.
package settings

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/fileperm"
)

type _key string

const (
	// Keys for various settings.
	// General settings keys.
	DataPathKey    _key = "data_path"    // string
	LogPathKey     _key = "log_path"     // string
	LogLevelKey    _key = "log_level"    // string
	CountryCodeKey _key = "country_code" // string
	LocaleKey      _key = "locale"       // string
	TimeZoneKey    _key = "time_zone"    // string, IANA name reported by the host app
	DeviceIDKey    _key = "device_id"    // string/int
	// PublicIPKey is the device's last detected public IP, truncated to its /24 (IPv4) or /32
	// (IPv6) so the stored value is enough for geolocation but doesn't identify the device.
	PublicIPKey _key = "public_ip" // string

	// Application behavior related keys.
	TelemetryKey _key = "telemetry_enabled" // bool

	// User account related keys.
	EmailKey         _key = "email"          // string
	UserIDKey        _key = "user_id"        // string
	UserLevelKey     _key = "user_level"     // string
	TokenKey         _key = "token"          // string
	JwtTokenKey      _key = "jwt_token"      // string
	DevicesKey       _key = "devices"        // []Device
	UserDataKey      _key = "user_data"      // [account.UserData]
	OAuthLoginKey    _key = "oauth_login"    // bool
	OAuthProviderKey _key = "oauth_provider" // string (e.g. "google", "apple", "email")

	// VPN related keys.
	SmartRoutingKey      _key = "smart_routing"       // bool
	SplitTunnelKey       _key = "split_tunnel"        // bool
	SplitTunnelPolicyKey _key = "split_tunnel_policy" // string ("exclude"|"include")
	AdBlockKey           _key = "ad_block"            // bool
	AutoConnectKey       _key = "auto_connect"        // bool
	PeerShareEnabledKey  _key = "peer_share_enabled"  // bool
	// PeerManualPortKey is the TCP port number the user has manually
	// forwarded on their router for the peer-proxy inbound (single-
	// port 1:1 NAT). Valid range is 1..65535; 0 means unset.
	PeerManualPortKey _key = "peer_manual_port" // int (0 = unset; 1..65535 = manual port)
	// UnboundedKey is the local opt-in (bool) for the Unbounded widget proxy.
	UnboundedKey      _key = "unbounded"       // bool
	SelectedServerKey _key = "selected_server" // [servers.Server] Server.Options is not stored

	PreferredLocationKey _key = "preferred_location" // [common.PreferredLocation]

	settingsFileName = "settings.json"
	// legacySettingsFileName is v9.0.x's name for the settings file.
	legacySettingsFileName = "local.json"

	settingsInvalidFileName = "settings.invalid.json"

	// migrationMarkerFileName lives beside settings.json rather than inside it, so losing the
	// settings file doesn't re-run a migration that already adopted an identity.
	migrationMarkerFileName = ".settings-migrated"

	// Development only keys
	ConfigFetchDisabledKey        _key = "config_fetch_disabled"     // bool
	FeatureOverridesKey           _key = "feature_overrides"         // string
	AdmissionRejectionDisabledKey _key = "admission_reject_disabled" // bool
)

var ErrNotExist = errors.New("key does not exist")

// errParseSettings marks a settings file that exists but isn't valid JSON. It
// distinguishes a recoverable invalid file (quarantine + start from defaults)
// from a genuine read failure, which stays fatal.
var errParseSettings = errors.New("settings file is not valid JSON")

func (k _key) String() string { return string(k) }

type settings struct {
	k           *koanf.Koanf
	initialized bool
	filePath    string
	mu          sync.Mutex
}

var k = &settings{
	k: newDefaults(),
}

func newDefaults() *koanf.Koanf {
	d := koanf.New(".")
	d.Set(UserLevelKey.String(), "free")
	return d
}

// InitSettings initializes the config for user settings. Until it succeeds, changes are kept in
// memory only.
func InitSettings(fileDir string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.initialized {
		return nil
	}
	if err := os.MkdirAll(fileDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %v", err)
	}
	k.filePath = filepath.Join(fileDir, settingsFileName)
	defer func() {
		if !k.initialized {
			k.filePath = ""
		}
	}()
	markerPath := filepath.Join(fileDir, migrationMarkerFileName)
	migrate := migrationPending(markerPath)
	var identity map[string]any
	if migrate {
		identity = legacyIdentity(fileDir, k.filePath)
	}
	fresh := false
	switch err := loadSettings(k.filePath); {
	case errors.Is(err, fs.ErrNotExist):
		slog.Warn("settings file not found", "path", k.filePath) // file may not have been created yet
		fresh = true
	case errors.Is(err, errParseSettings):
		// An invalid settings file must not be fatal; quarantine it before it's overwritten.
		slog.Error("Settings file is invalid; starting from defaults", "path", k.filePath, "error", err)
		quarantineInvalidSettings(k.filePath)
		fresh = true
	case err != nil:
		return fmt.Errorf("loading settings: %w", err)
	}
	switch {
	case identity != nil:
		err := updateLocked(func(kk *koanf.Koanf) error { return adoptIdentity(kk, identity) })
		if err != nil && fresh {
			return err
		}
		if err != nil {
			slog.Warn("legacy settings migration: saving adopted identity failed; will retry", "error", err)
			migrate = false
		}
	case fresh:
		if err := saveLocked(k.k); err != nil {
			return err
		}
	}
	if migrate {
		if err := atomicfile.WriteFile(markerPath, nil, fileperm.File); err != nil {
			slog.Warn("legacy settings migration: writing marker failed; will retry", "path", markerPath, "error", err)
		}
	}
	k.initialized = true
	return nil
}

// migrationPending reports whether the legacy migration has yet to complete. An unreadable marker
// counts as complete, because re-adopting a legacy identity can undo a logout.
func migrationPending(markerPath string) bool {
	_, err := os.Stat(markerPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("legacy settings migration: marker unreadable; skipping", "path", markerPath, "error", err)
	}
	return errors.Is(err, fs.ErrNotExist)
}

// candidateSource is one possible location of persisted user state. contents is v9.x JSON, or
// pre-9.x YAML translated to JSON.
type candidateSource struct {
	path     string
	contents []byte
	exists   bool
	label    string
}

// legacyIdentity returns the identity keys to adopt from settings written by older client
// versions, or nil when nothing should be adopted or canonicalPath can't be read. A legacy candidate
// that can't be read counts as missing.
//
// Legacy candidates in priority order:
//
//  1. <fileDir>/local.json                    — v9.0.x (renamed)
//  2. ${PUBLIC}\Lantern\data\local.json       — v9.0.x cross-dir, Windows only
//  3. pre-9.x platform-specific YAML
//  4. <fileDir>/data/settings.json            — v9.1.x (misplaced under data/)
//
// A canonical file holding a JSON object always wins, even without an account, because it is
// state this client already accepted. Otherwise a legacy candidate counts only if it holds a JSON
// object: pick the highest-priority one with user_level=="pro", or if none is pro, the
// highest-priority one with a non-zero user_id, so the device registration is kept.
func legacyIdentity(fileDir, canonicalPath string) map[string]any {
	switch raw, err := os.ReadFile(canonicalPath); {
	case err == nil:
		if p, err := (settingsJSON{}).Unmarshal(raw); err == nil && p != nil {
			return nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		// InitSettings fails on the same read before marking the migration complete.
		return nil
	}
	candidates := []candidateSource{
		{path: filepath.Join(fileDir, legacySettingsFileName), label: "v9.0.x local.json"},
		{path: filepath.Join(fileDir, "data", settingsFileName), label: "v9.1.x data/settings.json"},
	}
	for i := range candidates {
		b, err := os.ReadFile(candidates[i].path)
		switch {
		case err == nil:
			candidates[i].contents = b
			candidates[i].exists = true
		case errors.Is(err, fs.ErrNotExist):
		default:
			slog.Warn("legacy settings migration: read failed", "path", candidates[i].path, "error", err)
		}
	}
	// Optional candidates are inserted oldest-first, so newer generations rank higher.
	if yc := legacyYAMLCandidate(fileDir); yc.exists {
		candidates = append(candidates[:1], append([]candidateSource{yc}, candidates[1:]...)...)
	}
	// Cross-dir candidates are the same generation as local.json.
	if winExtras := windowsCrossDirCandidatesFn(fileDir); len(winExtras) > 0 {
		candidates = append(candidates[:1], append(winExtras, candidates[1:]...)...)
	}
	parsed := make([]map[string]any, len(candidates))
	for i, c := range candidates {
		if !c.exists {
			continue
		}
		p, err := (settingsJSON{}).Unmarshal(c.contents)
		if err != nil || p == nil {
			slog.Warn("legacy settings migration: ignoring invalid candidate", "path", c.path, "error", err)
			continue
		}
		parsed[i] = p
	}
	pickIdx := -1
	for i, p := range parsed {
		if p != nil && p[UserLevelKey.String()] == "pro" {
			pickIdx = i
			break
		}
	}
	if pickIdx == -1 {
		for i, p := range parsed {
			if hasAccount(p) {
				pickIdx = i
				break
			}
		}
	}
	if pickIdx == -1 {
		return nil
	}
	identity := make(map[string]any)
	for _, key := range identityKeys {
		if v, ok := parsed[pickIdx][key.String()]; ok {
			identity[key.String()] = v
		}
	}
	slog.Info("legacy settings migration: recovering persisted identity", "source", candidates[pickIdx].label)
	return identity
}

func hasAccount(settings map[string]any) bool {
	id := toString(settings[UserIDKey.String()])
	return id != "" && id != "0"
}

var identityKeys = []_key{
	UserIDKey, TokenKey, UserLevelKey, EmailKey, DeviceIDKey,
	JwtTokenKey, OAuthLoginKey, OAuthProviderKey,
}

// UserKeys are the settings that belong to the active user rather than the device.
var UserKeys = []_key{
	UserIDKey, TokenKey, UserLevelKey, EmailKey,
	JwtTokenKey, DevicesKey, UserDataKey, OAuthLoginKey, OAuthProviderKey,
}

func adoptIdentity(kk *koanf.Koanf, identity map[string]any) error {
	if toString(kk.Get(UserIDKey.String())) != toString(identity[UserIDKey.String()]) {
		for _, key := range UserKeys {
			kk.Delete(key.String())
		}
	}
	for key, v := range identity {
		if err := kk.Set(key, v); err != nil {
			return fmt.Errorf("could not set key %s: %w", key, err)
		}
	}
	return nil
}

// loadSettings applies the file to the in-memory settings.
//
// Koanf parses before taking its write lock, so malformed files leave the live settings untouched.
// Without opts the file merges over current settings; pass replaceAll when omitted keys must be
// dropped.
func loadSettings(path string, opts ...koanf.Option) error {
	contents, err := atomicfile.ReadFile(path)
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}
	if err := k.k.Load(rawbytes.Provider(contents), &settingsJSON{}, opts...); err != nil {
		return fmt.Errorf("parsing settings: %w", errors.Join(errParseSettings, err))
	}
	return nil
}

// replaceAll makes a koanf load overwrite rather than merge, so keys the source omits are dropped.
//
// Koanf re-flattens dest after this returns, so dest must be mutated in place; replacing the map
// header would silently lose the load.
func replaceAll(src, dest map[string]any) error {
	clear(dest)
	maps.Copy(dest, src)
	return nil
}

// quarantineInvalidSettings copies an invalid settings file aside to
// settings.invalid.json so a fresh default file can replace it in place while
// the original remains available for diagnostics.
func quarantineInvalidSettings(path string) {
	rawSettings, err := atomicfile.ReadFile(path)
	if err != nil {
		slog.Error("reading invalid settings for quarantine", "path", path, "error", err)
		return
	}
	invalidPath := filepath.Join(filepath.Dir(path), settingsInvalidFileName)
	if err := atomicfile.WriteFile(invalidPath, rawSettings, fileperm.File); err != nil {
		slog.Error("writing invalid settings copy", "path", invalidPath, "error", err)
		return
	}
	slog.Warn("quarantined invalid settings for diagnostics", "path", invalidPath)
}

func Get(key _key) any {
	return k.k.Get(key.String())
}

func GetString(key _key) string {
	return toString(Get(key))
}

func toString(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case float64:
		// Callers can store float64 IDs, which %v would render in scientific notation.
		return strconv.FormatInt(int64(v), 10)
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

func GetBool(key _key) bool {
	return k.k.Bool(key.String())
}

func GetInt(key _key) int {
	return k.k.Int(key.String())
}

func GetInt64(key _key) int64 {
	return k.k.Int64(key.String())
}

func GetFloat64(key _key) float64 {
	return k.k.Float64(key.String())
}

func GetStringSlice(key _key) []string {
	return k.k.Strings(key.String())
}

func GetDuration(key _key) time.Duration {
	return k.k.Duration(key.String())
}

func GetStruct(key _key, out any) error {
	return k.k.Unmarshal(key.String(), out)
}

func Exists(key _key) bool {
	return k.k.Exists(key.String())
}

// Set stores value under key. On error the previous value remains.
func Set(key _key, value any) error {
	return Patch(Settings{key: value})
}

// Clear removes keys. On error the previous values remain.
func Clear(keys ..._key) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return updateLocked(func(kk *koanf.Koanf) error {
		for _, key := range keys {
			kk.Delete(key.String())
		}
		return nil
	})
}

type Settings map[_key]any

// Diff returns the entries of s that are absent from s2 or hold a different value there.
func (s Settings) Diff(s2 Settings) Settings {
	diff := make(Settings)
	for k, v1 := range s {
		if v2, ok := s2[k]; !ok || !reflect.DeepEqual(v1, v2) {
			diff[k] = v1
		}
	}
	return diff
}

func GetAll() Settings {
	s := make(Settings)
	for key, value := range k.k.All() {
		s[_key(key)] = value
	}
	return s
}

// GetAllFor returns the values of keys that are set, omitting unset keys; with no keys it returns
// all settings.
func GetAllFor(keys ..._key) Settings {
	if len(keys) == 0 {
		return GetAll()
	}
	s := make(Settings)
	for _, key := range keys {
		if k.k.Exists(key.String()) {
			s[key] = k.k.Get(key.String())
		}
	}
	return s
}

// Patch applies updates all at once. On error the previous values remain.
func Patch(updates Settings) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return updateLocked(func(kk *koanf.Koanf) error {
		for key, value := range updates {
			if err := kk.Set(key.String(), value); err != nil {
				return fmt.Errorf("could not set key %s: %w", key, err)
			}
		}
		return nil
	})
}

// Apply is like [Patch], except the updates take effect in memory even when the save fails; the
// save error is still returned. It is for inputs the process recomputes on every start, such as
// paths and the host's telemetry consent, which must hold for the current run regardless.
func Apply(updates Settings) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for key, value := range updates {
		if err := k.k.Set(key.String(), value); err != nil {
			return fmt.Errorf("could not set key %s: %w", key, err)
		}
	}
	return saveLocked(k.k)
}

// updateLocked runs mutate on a copy of the settings, saves the copy, and only then makes it the
// live settings, so a failed save leaves them unchanged. It requires k.mu to be held.
func updateLocked(mutate func(*koanf.Koanf) error) error {
	staged := k.k.Copy()
	if err := mutate(staged); err != nil {
		return err
	}
	if err := saveLocked(staged); err != nil {
		return err
	}
	// Getters read k.k without k.mu, so replace its contents in place rather than the pointer.
	return k.k.Load(mapProvider(staged.Raw()), nil, koanf.WithMergeFunc(replaceAll))
}

// mapProvider is a koanf provider for an already-parsed settings map.
type mapProvider map[string]any

func (p mapProvider) Read() (map[string]any, error) { return p, nil }

func (mapProvider) ReadBytes() ([]byte, error) {
	return nil, errors.New("mapProvider has no raw bytes")
}

// saveLocked writes src to the settings file unless the file already holds it. It requires k.mu
// to be held.
func saveLocked(src *koanf.Koanf) error {
	if k.filePath == "" {
		return nil
	}
	out, err := src.Marshal(json.Parser())
	if err != nil {
		return fmt.Errorf("could not marshal koanf file: %w", err)
	}
	if current, err := atomicfile.ReadFile(k.filePath); err == nil && bytes.Equal(out, current) {
		return nil
	}
	if err := atomicfile.WriteFile(k.filePath, out, fileperm.File); err != nil {
		return fmt.Errorf("could not write koanf file: %w", err)
	}
	return nil
}

// Reset restores the default, uninitialized settings, primarily for testing purposes.
func Reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.k = newDefaults()
	k.filePath = ""
	k.initialized = false
}

// Reload re-reads the settings file, adopting values written by another process.
//
// Mobile keeps the tunnel in a separate process that persists to the same file, so long-lived
// callers must reload before using cross-process metadata. The file supersedes memory rather than
// merging over it, so keys removed by another process do not linger. A missing file is not an
// error, and failed reloads leave the current settings untouched.
func Reload() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.initialized {
		return errors.New("settings not initialized")
	}
	switch err := loadSettings(k.filePath, koanf.WithMergeFunc(replaceAll)); {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("reloading settings: %w", err)
	}
	return nil
}

func IsPro() bool {
	return strings.ToLower(GetString(UserLevelKey)) == "pro"
}

// Device is a machine registered to a user account (e.g. an Android phone or a Windows desktop).
type Device struct {
	ID   string
	Name string
}

func Devices() ([]Device, error) {
	devices := []Device{}
	err := GetStruct(DevicesKey, &devices)
	return devices, err
}
