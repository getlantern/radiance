// Package settings provides a simple interface for storing and retrieving user settings.
package settings

import (
	jsonpkg "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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
	DeviceIDKey    _key = "device_id"    // string/int

	// Application behavior related keys.
	TelemetryKey           _key = "telemetry_enabled"     // bool
	ConfigFetchDisabledKey _key = "config_fetch_disabled" // bool
	FeatureOverridesKey    _key = "feature_overrides"     // string

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
	SmartRoutingKey   _key = "smart_routing"   // bool
	SplitTunnelKey    _key = "split_tunnel"    // bool
	AdBlockKey        _key = "ad_block"        // bool
	AutoConnectKey    _key = "auto_connect"    // bool
	SelectedServerKey _key = "selected_server" // [servers.Server] Server.Options is not stored

	PreferredLocationKey _key = "preferred_location" // [common.PreferredLocation]

	settingsFileName = "settings.json"
	// legacySettingsFileName is what v9.0.x called the same file (it was
	// renamed in radiance PR #370). On upgrade from v9.0.x, the user's
	// persisted user_id / token / user_level live at <dataDir>/local.json;
	// migrateLegacySettingsIfNeeded reads it from there so Pro state
	// survives the rename.
	legacySettingsFileName = "local.json"
)

var ErrNotExist = errors.New("key does not exist")

func (k _key) String() string { return string(k) }

type settings struct {
	k           *koanf.Koanf
	initialized bool
	filePath    string
	mu          sync.Mutex
}

var k = &settings{
	k: koanf.New("."),
}

func init() {
	// set default values.
	k.k.Set(LocaleKey.String(), "fa-IR")
	k.k.Set(UserLevelKey.String(), "free")
}

// InitSettings initializes the config for user settings.
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
	migrateLegacySettingsIfNeeded(fileDir, k.filePath)
	switch err := loadSettings(k.filePath); {
	case errors.Is(err, fs.ErrNotExist):
		slog.Warn("settings file not found", "path", k.filePath) // file may not have been created yet
		return save()
	case err != nil:
		return fmt.Errorf("loading settings: %w", err)
	}
	k.initialized = true
	return nil
}

// candidateSource describes one possible location of persisted user
// state. `contents` is always JSON in the current canonical schema —
// either read directly (for the v9.x JSON files) or translated from a
// pre-9.x platform-specific YAML on the way in.
type candidateSource struct {
	path     string
	contents []byte
	exists   bool
	label    string
}

// migrateLegacySettingsIfNeeded recovers persisted user state written
// by older client versions. Candidate paths considered in priority
// order (highest first):
//
//  1. <fileDir>/settings.json         — current canonical path
//  2. <fileDir>/local.json            — what v9.0.x wrote (renamed in #370)
//  3. pre-9.x platform-specific YAML  — flashlight/lantern-client era:
//     macOS:   ~/.lantern/settings.yaml
//     Windows: %APPDATA%\Lantern\settings.yaml
//     Linux:   ~/.config/lantern/settings.yaml
//     iOS:     <fileDir>/userconfig.yaml
//     (Android stored its state in an encrypted SQLite that we can't
//     read here; that case needs a Kotlin-side migration outside this
//     package.)
//  4. <fileDir>/data/settings.json    — what v9.1.x wrote, due to a bug
//                                       in #370's setupDirectories that
//                                       appended an unconditional "/data"
//                                       suffix to the caller's data dir
//
// On the first launch of a fixed client, any subset of these may exist
// depending on the user's upgrade path. Pick whichever has
// user_level == "pro" so anyone who legitimately paid keeps their Pro
// state regardless of which file holds the good copy. If none have
// pro, prefer the higher-priority source so the user's identifiers
// (user_id, token, device_id) survive — losing Pro is recoverable,
// losing the device registration creates server-side orphans.
//
// Order rationale: canonical is most recent and authoritative.
// v9.0.x's local.json is the next-most-recent legitimate state.
// Pre-9.x YAML is older but trusted (real user state from
// flashlight/lantern-client). v9.1.x's nested file is most recent but
// known to be bugged (fresh device_id, possibly wrong user_id).
//
// Runs unconditionally — quick stat-and-read of a handful of small
// files; no-op for the vast majority of installs that don't have any
// of the legacy files.
func migrateLegacySettingsIfNeeded(fileDir, canonicalPath string) {
	candidates := []candidateSource{
		{path: canonicalPath, label: "canonical settings.json"},
		{path: filepath.Join(fileDir, legacySettingsFileName), label: "v9.0.x local.json"},
		// pre-9.x YAML is appended below after read+translate (only if
		// it exists for this platform) so the pickIdx loop sees it as
		// just another candidate.
		{path: filepath.Join(fileDir, "data", settingsFileName), label: "v9.1.x data/settings.json"},
	}
	for i := range candidates {
		b, err := os.ReadFile(candidates[i].path)
		switch {
		case err == nil:
			candidates[i].contents = b
			candidates[i].exists = true
		case errors.Is(err, fs.ErrNotExist):
			// Expected — file just isn't there. Treat as not-present.
		default:
			// Permission / I/O error — log it but don't bail outright. If
			// it's the canonical path that's unreadable for non-ENOENT
			// reasons, skip migration entirely so we don't try to write
			// over a file the OS is telling us we can't see; for legacy
			// or nested paths, treat the same as not-present.
			slog.Warn("legacy settings migration: read failed",
				"path", candidates[i].path, "error", err)
			if candidates[i].path == canonicalPath {
				return
			}
		}
	}
	// Insert the pre-9.x YAML candidate (translated to canonical JSON)
	// between local.json and data/settings.json. Only the platforms with
	// known pre-9.x file layouts return a non-empty result here.
	if yc := legacyYAMLCandidate(fileDir); yc.exists {
		// Splice in at index 2 (just before data/settings.json) so
		// priority order is canonical > local.json > pre-9.x > nested.
		candidates = append(candidates[:2], append([]candidateSource{yc}, candidates[2:]...)...)
	}

	// Pick: highest-priority file with user_level=="pro"; if none has pro,
	// highest-priority file that exists at all (with non-empty contents).
	pickIdx := -1
	for i, c := range candidates {
		if c.exists && userLevelInJSON(c.contents) == "pro" {
			pickIdx = i
			break
		}
	}
	if pickIdx == -1 {
		for i, c := range candidates {
			if c.exists {
				pickIdx = i
				break
			}
		}
	}
	if pickIdx == -1 {
		// Nothing on disk yet — fresh install, normal path. No-op.
		return
	}
	if candidates[pickIdx].path == canonicalPath {
		// Canonical already wins — no migration needed.
		return
	}
	writeMigrated(canonicalPath, candidates[pickIdx].contents, candidates[pickIdx].label)
}

// writeMigrated overwrites the canonical settings file with the recovered
// contents and logs the outcome. Uses atomicfile.WriteFile (the same
// mechanism the normal save path uses) so a crash mid-write can't leave
// a half-written settings.json on disk. Errors are logged-and-swallowed:
// if the write fails the caller falls through to the fresh-install path,
// which is a worse UX but not a corruption risk.
func writeMigrated(canonicalPath string, contents []byte, source string) {
	if err := atomicfile.WriteFile(canonicalPath, contents, fileperm.File); err != nil {
		slog.Warn("legacy settings migration: write failed",
			"dst", canonicalPath, "source", source, "error", err)
		return
	}
	slog.Info("legacy settings migration: recovered persisted state",
		"dst", canonicalPath, "source", source, "bytes", len(contents))
}

// userLevelInJSON returns the value of the "user_level" key from a JSON
// settings blob, or "" if the key is missing / the blob is malformed.
// Lightweight extraction so the migration doesn't need to load the full
// koanf state machine before we've decided which file to read.
func userLevelInJSON(contents []byte) string {
	var s struct {
		UserLevel string `json:"user_level"`
	}
	if err := jsonpkg.Unmarshal(contents, &s); err != nil {
		return ""
	}
	return s.UserLevel
}

func loadSettings(path string) error {
	contents, err := atomicfile.ReadFile(path)
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}
	kk := koanf.New(".")
	if err := kk.Load(rawbytes.Provider(contents), json.Parser()); err != nil {
		return fmt.Errorf("parsing settings: %w", err)
	}
	k.k = kk
	return nil
}

func Get(key _key) any {
	return k.k.Get(key.String())
}

func GetString(key _key) string {
	// JSON round-trip turns all numbers into float64 and since koanf uses Sprintf("%v") for string
	// conversion, large integers (i.e. userID) get converted to scientific notation (e.g. 3.87286618e+08)
	// so we handle float64 separately
	value := Get(key)
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case float64:
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

func Set(key _key, value any) error {
	err := k.k.Set(key.String(), value)
	if err != nil {
		return fmt.Errorf("could not set key %s: %w", key, err)
	}
	return save()
}

func Clear(key _key) {
	k.k.Delete(key.String())
}

type Settings map[_key]any

func (s Settings) Diff(s2 Settings) Settings {
	diff := make(Settings)
	for k, v1 := range s {
		if v2, ok := s2[k]; !ok || v1 != v2 {
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

func GetAllFor(keys ..._key) Settings {
	if len(keys) == 0 {
		return GetAll()
	}
	s := make(Settings)
	for _, key := range keys {
		s[key] = k.k.Get(key.String())
	}
	return s
}

// Patch takes a map of settings to update and applies them all at once.
func Patch(updates Settings) error {
	for key, value := range updates {
		if err := k.k.Set(_key(key).String(), value); err != nil {
			return fmt.Errorf("could not set key %s: %w", key, err)
		}
	}
	return save()
}

func save() error {
	out, err := k.k.Marshal(json.Parser())
	if err != nil {
		return fmt.Errorf("could not marshal koanf file: %w", err)
	}

	err = atomicfile.WriteFile(k.filePath, out, fileperm.File)
	if err != nil {
		return fmt.Errorf("could not write koanf file: %w", err)
	}
	return nil
}

// Reset clears the current settings in memory primarily for testing purposes.
func Reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.k = koanf.New(".")
	k.initialized = false
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
