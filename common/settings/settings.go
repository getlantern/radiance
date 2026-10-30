// Package settings provides a simple interface for storing and retrieving user settings.
package settings

import (
	"bytes"
	"context"
	jsonpkg "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/env"
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
	// PeerServePoolKey says whether the peer proxy also serves the general
	// pool ("help others too") rather than only its helper pairings. Unset
	// means true.
	PeerServePoolKey _key = "peer_serve_pool" // bool (unset = true)
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

// settings holds the live state as an immutable snapshot: readers load k
// without locking, and writers build the next snapshot under mu and swap it
// in, so a load can never mutate a map a reader is traversing.
type settings struct {
	k           atomic.Pointer[koanf.Koanf]
	initialized bool
	filePath    string
	mu          sync.Mutex
	// pending holds [Apply] writes that have not reached the file, reapplied on top of it until one
	// persists, so a later write cannot resurrect the state they replaced. Guarded by mu.
	pending map[string]pendingWrite
}

// pendingWrite is one key's write; deleted removes the key.
type pendingWrite struct {
	value   any
	deleted bool
}

var k = &settings{}

func init() {
	k.k.Store(defaultSnapshot())
}

func defaultSnapshot() *koanf.Koanf {
	next := koanf.New(".")
	next.Set(UserLevelKey.String(), "free")
	return next
}

func snapshot() *koanf.Koanf { return k.k.Load() }

// clone deep-copies src through a marshal round-trip, giving values the same
// types they would have after being persisted and loaded.
func clone(src *koanf.Koanf) (*koanf.Koanf, error) {
	out, err := src.Marshal(json.Parser())
	if err != nil {
		return nil, fmt.Errorf("could not marshal settings: %w", err)
	}
	next := koanf.New(".")
	if err := next.Load(rawbytes.Provider(out), &settingsJSON{}); err != nil {
		return nil, fmt.Errorf("could not copy settings: %w", err)
	}
	return next, nil
}

// InitSettings initializes the config for user settings. Before it succeeds, writes are held in
// memory only, and the file's values take precedence over them when it loads.
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
	next, err := clone(snapshot())
	if err != nil {
		return err
	}
	fresh := false
	switch err := loadInto(next, k.filePath); {
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
	if identity != nil {
		adopted, err := clone(next)
		if err == nil {
			err = adoptIdentity(adopted, identity)
		}
		if err == nil {
			err = save(adopted)
		}
		switch {
		case err == nil:
			next = adopted
			fresh = false
		case fresh:
			return err
		default:
			slog.Warn("legacy settings migration: saving adopted identity failed; will retry", "error", err)
			migrate = false
		}
	}
	if fresh {
		if err := save(next); err != nil {
			return err
		}
	}
	if migrate {
		if err := atomicfile.WriteFile(markerPath, nil, fileperm.File); err != nil {
			slog.Warn("legacy settings migration: writing marker failed; will retry", "path", markerPath, "error", err)
		}
	}
	k.k.Store(next)
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
	// Optional candidates go after local.json, inserted oldest-first so newer ones rank higher.
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

// readsHostProfile reports whether the migration may read legacy files outside the data dir. Under
// test they would import the developer's live Lantern account into the test's store.
func readsHostProfile() bool {
	return !testing.Testing() && !env.GetBool(env.Testing)
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

// loadInto merges the settings file at path into target. Koanf parses before
// merging, so a malformed file leaves target untouched.
func loadInto(target *koanf.Koanf, path string) error {
	contents, err := atomicfile.ReadFile(path)
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}
	if err := target.Load(rawbytes.Provider(contents), &settingsJSON{}); err != nil {
		return fmt.Errorf("parsing settings: %w", errors.Join(errParseSettings, err))
	}
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
	return snapshot().Get(key.String())
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
	return snapshot().Bool(key.String())
}

func GetInt(key _key) int {
	return snapshot().Int(key.String())
}

func GetInt64(key _key) int64 {
	return snapshot().Int64(key.String())
}

func GetFloat64(key _key) float64 {
	return snapshot().Float64(key.String())
}

func GetStringSlice(key _key) []string {
	return snapshot().Strings(key.String())
}

func GetDuration(key _key) time.Duration {
	return snapshot().Duration(key.String())
}

// GetStruct decodes the value at key into out by JSON field names, the form
// every value takes once it has passed through the settings file. A missing
// key is not an error and leaves out untouched.
func GetStruct(key _key, out any) error {
	raw := snapshot().Get(key.String())
	if raw == nil {
		return nil
	}
	// Not koanf's Unmarshal: it matches the koanf tag or the Go field name, so
	// a JSON name differing by more than case (user_level) is dropped.
	encoded, err := jsonpkg.Marshal(raw)
	if err != nil {
		return fmt.Errorf("could not encode value for %s: %w", key, err)
	}
	if err := jsonpkg.Unmarshal(encoded, out); err != nil {
		return fmt.Errorf("could not decode value for %s: %w", key, err)
	}
	return nil
}

func Exists(key _key) bool {
	return snapshot().Exists(key.String())
}

// mutate applies the writes returned by produce on top of the file's current contents, not this
// process's snapshot, so a write can't clobber keys another process changed; produce sees that
// same state and can decide against it. The writes take effect only once saved; on error the
// previous settings remain.
func mutate(produce func(next *koanf.Koanf) (map[string]pendingWrite, error)) error {
	return write(produce, false)
}

// write is [mutate], except that with keepUnsaved the writes take effect even when the file can't
// be read or saved, and are journaled until a save succeeds.
func write(produce func(next *koanf.Koanf) (map[string]pendingWrite, error), keepUnsaved bool) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	unlock := lockSettingsFile(k.filePath)
	defer unlock()
	next, baseErr := writeBase()
	if baseErr != nil {
		if !keepUnsaved {
			return baseErr
		}
		// Decide against the live snapshot instead of the unreadable file.
		var err error
		if next, err = clone(snapshot()); err != nil {
			return errors.Join(baseErr, err)
		}
		if err := applyWrites(next, k.pending); err != nil {
			return errors.Join(baseErr, err)
		}
	}
	updates, err := produce(next)
	if err != nil {
		return err
	}
	if err := applyWrites(next, updates); err != nil {
		return err
	}
	saveErr := baseErr
	if saveErr == nil {
		saveErr = save(next)
	}
	if saveErr != nil {
		if !keepUnsaved {
			return saveErr
		}
		if k.pending == nil {
			k.pending = make(map[string]pendingWrite)
		}
		for key, w := range updates {
			k.pending[key] = w
		}
	} else {
		k.pending = nil
	}
	k.k.Store(next)
	return saveErr
}

func writesProducer(updates map[string]pendingWrite) func(*koanf.Koanf) (map[string]pendingWrite, error) {
	return func(*koanf.Koanf) (map[string]pendingWrite, error) {
		return updates, nil
	}
}

func applyWrites(next *koanf.Koanf, updates map[string]pendingWrite) error {
	for key, w := range updates {
		if w.deleted {
			next.Delete(key)
			continue
		}
		if err := next.Set(key, w.value); err != nil {
			return fmt.Errorf("could not set key %s: %w", key, err)
		}
	}
	return nil
}

// writeBase returns the state a write starts from: the file's base state
// with this process's pending writes reapplied on top.
func writeBase() (*koanf.Koanf, error) {
	next, err := diskBase()
	if err != nil {
		return nil, err
	}
	if err := applyWrites(next, k.pending); err != nil {
		return nil, err
	}
	return next, nil
}

// diskBase returns the file's contents over the defaults. A missing or
// corrupt file falls back to the current snapshot, which turns a corrupt
// file back into valid state on the next write; any other read error is
// returned.
func diskBase() (*koanf.Koanf, error) {
	if k.filePath == "" {
		return clone(snapshot())
	}
	next := defaultSnapshot()
	switch err := loadInto(next, k.filePath); {
	case err == nil:
		return next, nil
	case errors.Is(err, fs.ErrNotExist):
		return clone(snapshot())
	case errors.Is(err, errParseSettings):
		slog.Warn("settings file invalid; overwriting with current settings", "path", k.filePath, "error", err)
		return clone(snapshot())
	default:
		return nil, err
	}
}

// lockSettingsFile takes the lock that serializes settings writers across
// processes, returning its release func. Failure to lock degrades to
// in-process serialization only, rather than failing the write.
func lockSettingsFile(path string) func() {
	if path == "" {
		return func() {}
	}
	fl := flock.New(path + ".lock")
	// A healthy holder keeps the lock for one read-modify-write; a holder
	// that waits longer is suspended (iOS freezes apps mid-write), and
	// startup paths write several keys against a ~7.5s iOS tunnel budget,
	// so waiting longer per write risks the whole process.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	locked, err := fl.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil || !locked {
		slog.Warn("settings file lock unavailable; writing without it", "path", path, "error", err)
		return func() {}
	}
	return func() {
		if err := fl.Unlock(); err != nil {
			slog.Warn("releasing settings file lock", "path", path, "error", err)
		}
	}
}

// Set stores value under key. On error the previous value remains.
func Set(key _key, value any) error {
	return mutate(writesProducer(map[string]pendingWrite{key.String(): {value: value}}))
}

// SetIfAbsent persists value under key only when the key has no value yet,
// deciding against the file's state under the settings write lock.
func SetIfAbsent(key _key, value any) error {
	return mutate(func(next *koanf.Koanf) (map[string]pendingWrite, error) {
		if next.Exists(key.String()) {
			return nil, nil
		}
		return map[string]pendingWrite{key.String(): {value: value}}, nil
	})
}

// ReplaceString sets key to "to" only while its current value is "from",
// deciding against the file's state under the settings write lock.
func ReplaceString(key _key, from, to string) error {
	return mutate(func(next *koanf.Koanf) (map[string]pendingWrite, error) {
		if next.String(key.String()) != from {
			return nil, nil
		}
		return map[string]pendingWrite{key.String(): {value: to}}, nil
	})
}

// Clear removes keys. On error the previous values remain.
func Clear(keys ..._key) error {
	updates := make(map[string]pendingWrite, len(keys))
	for _, key := range keys {
		updates[key.String()] = pendingWrite{deleted: true}
	}
	return mutate(writesProducer(updates))
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
	for key, value := range snapshot().All() {
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
	cur := snapshot()
	s := make(Settings)
	for _, key := range keys {
		if cur.Exists(key.String()) {
			s[key] = cur.Get(key.String())
		}
	}
	return s
}

// Patch applies updates all at once. On error the previous values remain.
func Patch(updates Settings) error {
	return mutate(writesProducer(valueWrites(updates)))
}

// Apply is like [Patch], except the updates take effect even when the settings file can't be read
// or saved, and that error is returned. Unsaved updates are reapplied over the file, including by
// [Reload], until a later write saves them.
func Apply(updates Settings) error {
	return write(writesProducer(valueWrites(updates)), true)
}

func valueWrites(updates Settings) map[string]pendingWrite {
	writes := make(map[string]pendingWrite, len(updates))
	for key, value := range updates {
		writes[key.String()] = pendingWrite{value: value}
	}
	return writes
}

// save writes next to the settings file unless the file already holds it. It does nothing until
// [InitSettings] sets the file path, and requires k.mu to be held.
func save(next *koanf.Koanf) error {
	if k.filePath == "" {
		return nil
	}
	out, err := next.Marshal(json.Parser())
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
	k.k.Store(defaultSnapshot())
	k.pending = nil
	k.filePath = ""
	k.initialized = false
}

// Reload re-reads the settings file, adopting values written by another process.
//
// Mobile keeps the tunnel in a separate process that persists to the same file, so long-lived
// callers must reload before using cross-process metadata. The file supersedes memory rather than
// merging over it, so keys removed by another process do not linger; this process's unpersisted
// writes are reapplied on top. A missing file is not an error, and failed reloads leave the
// current settings untouched.
func Reload() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.initialized {
		return errors.New("settings not initialized")
	}
	next := defaultSnapshot()
	switch err := loadInto(next, k.filePath); {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("reloading settings: %w", err)
	}
	if err := applyWrites(next, k.pending); err != nil {
		return err
	}
	k.k.Store(next)
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
