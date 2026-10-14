package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/getlantern/radiance/common/env"
)

func TestWindowsCrossDirCandidates(t *testing.T) {
	t.Run("returns nil on non-Windows", func(t *testing.T) {
		// On macOS/Linux the function must short-circuit regardless of
		// %PUBLIC% being set — this is the implicit GOOS gate that keeps
		// the cross-dir scan from misfiring elsewhere.
		if runtime.GOOS == "windows" {
			t.Skip("test only meaningful on non-Windows hosts")
		}
		t.Setenv("PUBLIC", t.TempDir())
		assert.Nil(t, windowsCrossDirCandidates("/some/dir"))
	})

	t.Run("returns nil when %PUBLIC% is unset", func(t *testing.T) {
		t.Setenv("PUBLIC", "")
		assert.Nil(t, windowsCrossDirCandidates("/some/dir"))
	})

	t.Run("readWindowsCrossDirCandidates picks up local.json", func(t *testing.T) {
		dir := t.TempDir()
		body := []byte(`{"user_level":"pro","user_id":1}`)
		require.NoError(t, os.WriteFile(filepath.Join(dir, legacySettingsFileName), body, 0o644))

		cs := readWindowsCrossDirCandidates(dir)
		require.Len(t, cs, 1, "expected one candidate when only local.json exists")
		assert.True(t, cs[0].exists)
		assert.Equal(t, body, cs[0].contents)
		assert.Contains(t, cs[0].label, "v9.0.x Windows", "label must identify the cross-dir source")
	})

	t.Run("readWindowsCrossDirCandidates ignores settings.json", func(t *testing.T) {
		dir := t.TempDir()
		local := []byte(`{"user_level":"pro","user_id":1}`)
		require.NoError(t, os.WriteFile(filepath.Join(dir, legacySettingsFileName), local, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, settingsFileName), []byte(`{"user_id":2}`), 0o644))

		cs := readWindowsCrossDirCandidates(dir)
		require.Len(t, cs, 1)
		assert.Equal(t, local, cs[0].contents)
	})

	t.Run("readWindowsCrossDirCandidates returns empty when dir is empty", func(t *testing.T) {
		assert.Empty(t, readWindowsCrossDirCandidates(t.TempDir()))
	})

	t.Run("samePathWindows is case-insensitive", func(t *testing.T) {
		// This is the load-bearing property of the guard — NTFS is
		// case-insensitive by default, so callers may supply equivalent
		// paths with different casing. Portable across OSes.
		assert.True(t, samePathWindows("/tmp/foo", "/tmp/FOO"))
		assert.True(t, samePathWindows(`C:\Users\Public`, `c:\users\public`))
		assert.False(t, samePathWindows("/tmp/foo", "/tmp/bar"))
	})

	t.Run("samePathWindows normalizes Windows separators (Windows only)", func(t *testing.T) {
		// On Windows, filepath.Clean folds `\` and `/` into the native
		// separator and strips trailing slashes. On Unix-style hosts
		// these characters are treated literally, so the assertion is
		// meaningless off-Windows. The function itself is only called
		// from windowsCrossDirCandidates, which short-circuits unless
		// GOOS=="windows" — i.e. the cases below only matter in the
		// environment they're exercised.
		if runtime.GOOS != "windows" {
			t.Skip("Windows-only path-cleaning semantics")
		}
		assert.True(t, samePathWindows(`C:\Users\Public\Lantern\data`, `C:/Users/Public/Lantern/data`))
		assert.True(t, samePathWindows(`C:\Users\Public\Lantern\data\`, `C:\Users\Public\Lantern\data`))
		assert.False(t, samePathWindows(`C:\Users\Public\Lantern\data`, `C:\Users\Public\Lantern\other`))
	})
}

// isolateDiscovery stops InitSettings from reading the host's own legacy settings files.
func isolateDiscovery(t *testing.T) {
	t.Helper()
	prevYAML, prevWin := legacyYAMLPathFn, windowsCrossDirCandidatesFn
	legacyYAMLPathFn = func(string) (string, string) { return "", "" }
	windowsCrossDirCandidatesFn = func(string) []candidateSource { return nil }
	t.Cleanup(func() {
		legacyYAMLPathFn, windowsCrossDirCandidatesFn = prevYAML, prevWin
	})
}

func initAt(t *testing.T, dir string) {
	t.Helper()
	Reset()
	t.Cleanup(Reset)
	require.NoError(t, InitSettings(dir))
}

// readOnlyDir makes dir readable but not writable until the returned func restores it, skipping the
// test where that cannot be enforced.
func readOnlyDir(t *testing.T, dir string) (restore func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions differ on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes regardless of mode")
	}
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	return func() { require.NoError(t, os.Chmod(dir, 0o755)) }
}

// moveAside makes dir unavailable until the returned func moves it back.
func moveAside(t *testing.T, dir string) (restore func()) {
	t.Helper()
	aside := dir + ".aside"
	require.NoError(t, os.Rename(dir, aside))
	return func() { require.NoError(t, os.Rename(aside, dir)) }
}

func TestInitSettings(t *testing.T) {
	isolateDiscovery(t)

	t.Run("existing valid config file", func(t *testing.T) {
		tempDir := t.TempDir()
		path := filepath.Join(tempDir, settingsFileName)
		content := []byte(`{"locale": "en-US", "country_code": "US"}`)
		require.NoError(t, os.WriteFile(path, content, 0644), "failed to create test config file")

		initAt(t, tempDir)
		assert.Equal(t, "en-US", Get(LocaleKey))
		assert.Equal(t, "US", Get(CountryCodeKey))
	})

	t.Run("invalid config file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), settingsFileName)
		content := []byte(`{invalid json}`)
		require.NoError(t, os.WriteFile(path, content, 0644), "failed to create test config file")
		require.Error(t, loadSettings(path), "expected error for invalid config file")
	})

	t.Run("invalid config file recovers", func(t *testing.T) {
		tempDir := t.TempDir()
		path := filepath.Join(tempDir, settingsFileName)
		require.NoError(t, os.WriteFile(path, []byte(`{invalid json}`), 0644))

		initAt(t, tempDir)

		invalidPath := filepath.Join(tempDir, settingsInvalidFileName)
		assert.FileExists(t, invalidPath, "the invalid file must be preserved for diagnostics")
		assert.NoError(t, loadSettings(path), "settings.json must be rewritten with a parseable default")
	})

	t.Run("changes before InitSettings stay in memory", func(t *testing.T) {
		Reset()
		t.Cleanup(Reset)

		require.NoError(t, Set(LocaleKey, "en-US"))

		assert.Equal(t, "en-US", GetString(LocaleKey))
	})

	t.Run("Reset restores defaults", func(t *testing.T) {
		initAt(t, t.TempDir())
		require.NoError(t, Set(UserLevelKey, "pro"))
		Reset()
		assert.Equal(t, "free", GetString(UserLevelKey))
		assert.Equal(t, "fa-IR", GetString(LocaleKey))
	})
}

func TestLegacyMigration(t *testing.T) {
	isolateDiscovery(t)

	writeNested := func(t *testing.T, dir string, contents string) {
		t.Helper()
		nd := filepath.Join(dir, "data")
		require.NoError(t, os.MkdirAll(nd, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(nd, settingsFileName), []byte(contents), 0o644))
	}
	writeLegacy := func(t *testing.T, dir string, contents string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, legacySettingsFileName), []byte(contents), 0o644))
	}
	writeCanonical := func(t *testing.T, dir string, contents string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, settingsFileName), []byte(contents), 0o644))
	}
	useCrossDir := func(t *testing.T, v90xDir string) {
		t.Helper()
		prev := windowsCrossDirCandidatesFn
		windowsCrossDirCandidatesFn = func(string) []candidateSource {
			return readWindowsCrossDirCandidates(v90xDir)
		}
		t.Cleanup(func() { windowsCrossDirCandidatesFn = prev })
	}
	useYAML := func(t *testing.T, dir, contents string) {
		t.Helper()
		yamlPath := filepath.Join(dir, "fake-pre9x-settings.yaml")
		require.NoError(t, os.WriteFile(yamlPath, []byte(contents), 0o644))
		prev := legacyYAMLPathFn
		legacyYAMLPathFn = func(string) (string, string) { return yamlPath, "desktop" }
		t.Cleanup(func() { legacyYAMLPathFn = prev })
	}
	// onDisk reads back the saved settings, so assertions cover what the next start sees.
	onDisk := func(t *testing.T, dir string) map[string]any {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, settingsFileName))
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal(b, &got))
		return got
	}

	t.Run("v9.0.x local.json recovered when canonical is missing", func(t *testing.T) {
		tempDir := t.TempDir()
		writeLegacy(t, tempDir, `{"user_id": 3580849, "user_level": "pro", "token": "abc"}`)

		initAt(t, tempDir)

		assert.Equal(t, "3580849", GetString(UserIDKey))
		assert.Equal(t, "pro", GetString(UserLevelKey))
		assert.Equal(t, "abc", GetString(TokenKey))
		assert.Equal(t, "abc", onDisk(t, tempDir)["token"], "adopted identity must be saved")
		assert.FileExists(t, filepath.Join(tempDir, migrationMarkerFileName))
	})

	t.Run("v9.1.x nested file recovered when canonical is missing", func(t *testing.T) {
		tempDir := t.TempDir()
		writeNested(t, tempDir, `{"user_id": 135809562, "user_level": "pro", "device_id": "abc"}`)

		initAt(t, tempDir)

		assert.Equal(t, "135809562", GetString(UserIDKey))
		assert.Equal(t, "abc", GetString(DeviceIDKey))
	})

	t.Run("v9.0.x local.json wins over v9.1.x expired nested", func(t *testing.T) {
		tempDir := t.TempDir()
		writeLegacy(t, tempDir, `{"user_id": 1, "user_level": "pro"}`)
		writeNested(t, tempDir, `{"user_id": 999, "user_level": "expired"}`)

		initAt(t, tempDir)

		assert.Equal(t, "1", GetString(UserIDKey))
		assert.True(t, IsPro())
	})

	t.Run("canonical-pro wins over nested-expired", func(t *testing.T) {
		tempDir := t.TempDir()
		writeCanonical(t, tempDir, `{"user_id": 1, "user_level": "pro"}`)
		writeNested(t, tempDir, `{"user_id": 999, "user_level": "expired"}`)

		initAt(t, tempDir)

		assert.Equal(t, "1", GetString(UserIDKey))
		assert.True(t, IsPro())
	})

	t.Run("nested-pro wins over canonical-expired and legacy-expired", func(t *testing.T) {
		// e.g., user paid via Shepherd while on v9.1.x, so the nested file
		// legitimately holds pro state.
		tempDir := t.TempDir()
		writeCanonical(t, tempDir, `{"user_id": 1, "user_level": "expired", "token": "t1", "jwt_token": "j1", "locale": "en-US"}`)
		writeLegacy(t, tempDir, `{"user_id": 1, "user_level": "expired"}`)
		writeNested(t, tempDir, `{"user_id": 2, "user_level": "pro"}`)

		initAt(t, tempDir)

		assert.Equal(t, "2", GetString(UserIDKey))
		assert.True(t, IsPro())
		assert.False(t, Exists(TokenKey), "the replaced user's token must not carry over")
		assert.False(t, Exists(JwtTokenKey), "the replaced user's JWT must not carry over")
		assert.Equal(t, "en-US", GetString(LocaleKey), "non-identity settings are kept")
	})

	t.Run("same user keeps its credentials when a legacy file restores pro", func(t *testing.T) {
		tempDir := t.TempDir()
		writeCanonical(t, tempDir, `{"user_id": 1, "user_level": "expired", "jwt_token": "j1"}`)
		writeLegacy(t, tempDir, `{"user_id": 1, "user_level": "pro"}`)

		initAt(t, tempDir)

		assert.True(t, IsPro())
		assert.Equal(t, "j1", GetString(JwtTokenKey))
	})

	t.Run("all-pro: canonical wins (most recent deliberate state)", func(t *testing.T) {
		tempDir := t.TempDir()
		writeCanonical(t, tempDir, `{"user_id": 1, "user_level": "pro"}`)
		writeLegacy(t, tempDir, `{"user_id": 2, "user_level": "pro"}`)
		writeNested(t, tempDir, `{"user_id": 3, "user_level": "pro"}`)

		initAt(t, tempDir)

		assert.Equal(t, "1", GetString(UserIDKey))
	})

	t.Run("none have pro: legacy wins over nested when canonical missing", func(t *testing.T) {
		// User identifiers must survive even when Pro state is non-pro,
		// to keep the device registration intact server-side.
		tempDir := t.TempDir()
		writeLegacy(t, tempDir, `{"user_id": 1, "user_level": "free", "token": "abc"}`)
		writeNested(t, tempDir, `{"user_id": 2, "user_level": "free", "token": "xyz"}`)

		initAt(t, tempDir)

		assert.Equal(t, "1", GetString(UserIDKey))
		assert.Equal(t, "abc", GetString(TokenKey))
	})

	t.Run("adopts the recovered account's OAuth state", func(t *testing.T) {
		tempDir := t.TempDir()
		writeNested(t, tempDir, `{"user_id": 3, "user_level": "pro", "jwt_token": "jwt", "oauth_login": true, "oauth_provider": "apple"}`)

		initAt(t, tempDir)

		assert.Equal(t, "3", GetString(UserIDKey))
		assert.Equal(t, "jwt", GetString(JwtTokenKey))
		assert.True(t, GetBool(OAuthLoginKey))
		assert.Equal(t, "apple", GetString(OAuthProviderKey))
	})

	t.Run("a canonical file without a user id doesn't outrank a legacy identity", func(t *testing.T) {
		tempDir := t.TempDir()
		writeCanonical(t, tempDir, `{"locale": "en-US", "device_id": "dev"}`)
		writeLegacy(t, tempDir, `{"user_id": 7, "user_level": "free", "token": "tok"}`)

		initAt(t, tempDir)

		assert.Equal(t, "7", GetString(UserIDKey))
		assert.Equal(t, "en-US", GetString(LocaleKey))
		assert.Equal(t, "dev", GetString(DeviceIDKey))
	})

	t.Run("an unreadable legacy file leaves the migration pending", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("permission semantics differ on windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root reads files regardless of mode")
		}
		tempDir := t.TempDir()
		writeLegacy(t, tempDir, `{"user_id": 7, "user_level": "free", "token": "tok"}`)
		legacy := filepath.Join(tempDir, legacySettingsFileName)
		require.NoError(t, os.Chmod(legacy, 0o000))
		t.Cleanup(func() { _ = os.Chmod(legacy, 0o644) })

		initAt(t, tempDir)
		assert.False(t, Exists(UserIDKey))
		assert.NoFileExists(t, filepath.Join(tempDir, migrationMarkerFileName))
		require.NoError(t, Set(DeviceIDKey, "dev"))

		require.NoError(t, os.Chmod(legacy, 0o644))
		initAt(t, tempDir)
		assert.Equal(t, "7", GetString(UserIDKey))
		assert.FileExists(t, filepath.Join(tempDir, migrationMarkerFileName))
	})

	t.Run("adopts identity keys only", func(t *testing.T) {
		tempDir := t.TempDir()
		writeLegacy(t, tempDir, `{"user_id": 1, "user_level": "pro", "peer_share_enabled": true, "locale": "ru-RU"}`)

		initAt(t, tempDir)

		assert.Equal(t, "1", GetString(UserIDKey))
		assert.False(t, Exists(PeerShareEnabledKey))
		assert.Equal(t, "fa-IR", GetString(LocaleKey))
	})

	t.Run("fresh install writes defaults and the marker", func(t *testing.T) {
		tempDir := t.TempDir()

		initAt(t, tempDir)

		assert.Equal(t, "free", GetString(UserLevelKey))
		assert.FileExists(t, filepath.Join(tempDir, settingsFileName))
		assert.FileExists(t, filepath.Join(tempDir, migrationMarkerFileName))
	})

	t.Run("a completed migration never resurrects a cleared account", func(t *testing.T) {
		tempDir := t.TempDir()
		writeLegacy(t, tempDir, `{"user_id": 111, "user_level": "pro", "token": "OLD-PRO-TOKEN", "email": "old@example.com"}`)
		initAt(t, tempDir)
		require.Equal(t, "111", GetString(UserIDKey))

		require.NoError(t, Clear(UserIDKey, TokenKey, UserLevelKey, EmailKey))
		require.NoError(t, Patch(Settings{UserIDKey: 222, UserLevelKey: "free"}))
		initAt(t, tempDir)

		assert.Equal(t, "222", GetString(UserIDKey))
		assert.Equal(t, "free", GetString(UserLevelKey))
		assert.False(t, Exists(TokenKey))

		require.NoError(t, Set(UserLevelKey, "expired"))
		initAt(t, tempDir)
		assert.Equal(t, "expired", GetString(UserLevelKey))
	})

	t.Run("a missing canonical file after migration starts fresh", func(t *testing.T) {
		tempDir := t.TempDir()
		writeLegacy(t, tempDir, `{"user_id": 111, "user_level": "pro"}`)
		initAt(t, tempDir)
		require.NoError(t, os.Remove(filepath.Join(tempDir, settingsFileName)))

		initAt(t, tempDir)

		assert.False(t, Exists(UserIDKey))
	})

	t.Run("malformed canonical loses to a valid legacy file", func(t *testing.T) {
		tempDir := t.TempDir()
		writeCanonical(t, tempDir, `{invalid json}`)
		writeLegacy(t, tempDir, `{"user_id": 7, "user_level": "free", "token": "tok"}`)

		initAt(t, tempDir)

		assert.Equal(t, "7", GetString(UserIDKey))
		assert.Equal(t, "tok", GetString(TokenKey))
		assert.FileExists(t, filepath.Join(tempDir, settingsInvalidFileName))
	})

	t.Run("malformed legacy candidate is ignored", func(t *testing.T) {
		tempDir := t.TempDir()
		writeLegacy(t, tempDir, `{"user_level": "pro"`)
		writeNested(t, tempDir, `{"user_id": 5, "user_level": "free"}`)

		initAt(t, tempDir)

		assert.Equal(t, "5", GetString(UserIDKey))
	})

	t.Run("a failed save is retried on the next start", func(t *testing.T) {
		tempDir := t.TempDir()
		writeCanonical(t, tempDir, `{"user_id": 1, "user_level": "expired"}`)
		writeLegacy(t, tempDir, `{"user_id": 2, "user_level": "pro"}`)
		restore := readOnlyDir(t, tempDir)

		initAt(t, tempDir)
		assert.Equal(t, "1", GetString(UserIDKey), "an unsaved adoption must not take effect")
		assert.NoFileExists(t, filepath.Join(tempDir, migrationMarkerFileName))

		restore()
		initAt(t, tempDir)
		assert.Equal(t, "2", GetString(UserIDKey))
		assert.FileExists(t, filepath.Join(tempDir, migrationMarkerFileName))
	})

	t.Run("a new canonical file carries the adopted identity", func(t *testing.T) {
		tempDir := t.TempDir()
		writeLegacy(t, tempDir, `{"user_id": 7, "user_level": "free", "token": "tok"}`)

		initAt(t, tempDir)

		assert.Equal(t, float64(7), onDisk(t, tempDir)["user_id"])
	})

	t.Run("a failed first write leaves a non-pro legacy identity recoverable", func(t *testing.T) {
		tempDir := t.TempDir()
		writeLegacy(t, tempDir, `{"user_id": 7, "user_level": "free", "token": "tok"}`)
		restore := readOnlyDir(t, tempDir)

		Reset()
		t.Cleanup(Reset)
		require.Error(t, InitSettings(tempDir))
		assert.NoFileExists(t, filepath.Join(tempDir, settingsFileName))

		restore()
		initAt(t, tempDir)
		assert.Equal(t, "7", GetString(UserIDKey))
	})

	t.Run("pre-9.x desktop YAML recovered when no JSON candidates exist", func(t *testing.T) {
		tempDir := t.TempDir()
		useYAML(t, tempDir, `userID: 3580849
deviceID: legacy-device-id
userPro: true
userToken: legacy-token
emailAddress: derek@example.com
`)

		initAt(t, tempDir)

		assert.Equal(t, "3580849", GetString(UserIDKey))
		assert.Equal(t, "legacy-device-id", GetString(DeviceIDKey))
		assert.Equal(t, "pro", GetString(UserLevelKey))
		assert.Equal(t, "legacy-token", GetString(TokenKey))
		assert.Equal(t, "derek@example.com", GetString(EmailKey))
	})

	t.Run("v9.0.x local.json beats pre-9.x YAML", func(t *testing.T) {
		tempDir := t.TempDir()
		useYAML(t, tempDir, `userID: 1
userPro: true
userToken: legacy-token
`)
		writeLegacy(t, tempDir, `{"user_id": 2, "user_level": "pro", "token": "v9.0-token"}`)

		initAt(t, tempDir)

		assert.Equal(t, "2", GetString(UserIDKey))
		assert.Equal(t, "v9.0-token", GetString(TokenKey))
	})

	t.Run("pre-9.x YAML beats v9.1.x bugged nested file", func(t *testing.T) {
		tempDir := t.TempDir()
		useYAML(t, tempDir, `userID: 1
userPro: true
userToken: legacy-token
`)
		writeNested(t, tempDir, `{"user_id": 999, "user_level": "expired"}`)

		initAt(t, tempDir)

		assert.Equal(t, "pro", GetString(UserLevelKey))
		assert.Equal(t, "legacy-token", GetString(TokenKey))
	})

	t.Run("iOS userconfig.yaml recovered when canonical is missing", func(t *testing.T) {
		if runtime.GOOS != "ios" {
			t.Skip("iOS-only path: legacy YAML elsewhere is OS-specific")
		}
		prev := legacyYAMLPathFn
		legacyYAMLPathFn = legacyYAMLPath
		t.Cleanup(func() { legacyYAMLPathFn = prev })
		tempDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(tempDir, "userconfig.yaml"), []byte(`UserID: 7777
DeviceID: ios-device
Token: tok
`), 0o644))

		initAt(t, tempDir)

		assert.Equal(t, "7777", GetString(UserIDKey))
		assert.Equal(t, "ios-device", GetString(DeviceIDKey))
	})

	t.Run("Windows v9.0.x cross-dir local.json recovered when canonical/same-dir/nested all missing", func(t *testing.T) {
		tempDir := t.TempDir()
		v90xDir := filepath.Join(tempDir, "windows-v90x")
		require.NoError(t, os.MkdirAll(v90xDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(v90xDir, legacySettingsFileName),
			[]byte(`{"user_id": 195646669, "user_level": "pro", "token": "preserved"}`), 0o644))
		useCrossDir(t, v90xDir)

		initAt(t, tempDir)

		assert.Equal(t, "195646669", GetString(UserIDKey))
		assert.Equal(t, "preserved", GetString(TokenKey))
	})

	t.Run("Windows cross-dir settings.json is never adopted", func(t *testing.T) {
		tempDir := t.TempDir()
		v90xDir := filepath.Join(tempDir, "windows-v90x")
		require.NoError(t, os.MkdirAll(v90xDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(v90xDir, settingsFileName),
			[]byte(`{"user_id": 42, "user_level": "pro", "peer_share_enabled": true}`), 0o644))
		useCrossDir(t, v90xDir)

		initAt(t, tempDir)

		assert.False(t, Exists(UserIDKey))
		assert.False(t, Exists(PeerShareEnabledKey))
	})

	t.Run("Windows v9.0.x cross-dir loses to same-dir v9.0.x local.json", func(t *testing.T) {
		tempDir := t.TempDir()
		v90xDir := filepath.Join(tempDir, "windows-v90x")
		require.NoError(t, os.MkdirAll(v90xDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(v90xDir, legacySettingsFileName),
			[]byte(`{"user_id": 999, "user_level": "pro"}`), 0o644))
		writeLegacy(t, tempDir, `{"user_id": 1, "user_level": "pro", "token": "same-dir"}`)
		useCrossDir(t, v90xDir)

		initAt(t, tempDir)

		assert.Equal(t, "1", GetString(UserIDKey))
		assert.Equal(t, "same-dir", GetString(TokenKey))
	})

	t.Run("Windows v9.0.x cross-dir-pro beats v9.1.x nested-expired", func(t *testing.T) {
		tempDir := t.TempDir()
		v90xDir := filepath.Join(tempDir, "windows-v90x")
		require.NoError(t, os.MkdirAll(v90xDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(v90xDir, legacySettingsFileName),
			[]byte(`{"user_id": 195646669, "user_level": "pro"}`), 0o644))
		writeNested(t, tempDir, `{"user_id": 99, "user_level": "expired"}`)
		useCrossDir(t, v90xDir)

		initAt(t, tempDir)

		assert.Equal(t, "195646669", GetString(UserIDKey))
		assert.True(t, IsPro())
	})

	t.Run("unreadable canonical (non-ENOENT) skips migration", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("permission semantics differ on windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root reads files regardless of mode")
		}
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		writeCanonical(t, tempDir, `{"user_level": "expired"}`)
		require.NoError(t, os.Chmod(canonical, 0o000))
		t.Cleanup(func() { _ = os.Chmod(canonical, 0o644) })
		writeLegacy(t, tempDir, `{"user_id": 1, "user_level": "pro"}`)

		Reset()
		t.Cleanup(Reset)
		assert.Error(t, InitSettings(tempDir))

		require.NoError(t, os.Chmod(canonical, 0o644))
		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, `{"user_level": "expired"}`, string(got),
			"canonical with non-ENOENT read error should be left alone")
		assert.NoFileExists(t, filepath.Join(tempDir, migrationMarkerFileName))
	})
}

func TestDiff(t *testing.T) {
	curr := Settings{
		DevicesKey:          []any{map[string]any{"ID": "a"}},
		UserDataKey:         nil,
		LocaleKey:           "en-US",
		FeatureOverridesKey: map[string]any{"x": true},
	}
	updates := Settings{
		DevicesKey:          []any{map[string]any{"ID": "a"}},
		UserDataKey:         nil,
		LocaleKey:           "fa-IR",
		FeatureOverridesKey: map[string]any{"x": false},
		EmailKey:            nil,
	}

	diff := updates.Diff(curr)

	assert.Equal(t, Settings{
		LocaleKey:           "fa-IR",
		FeatureOverridesKey: map[string]any{"x": false},
		EmailKey:            nil,
	}, diff, "equal slices and nulls drop out; a null for an absent key stays")
}

func TestGetAllForOmitsAbsentKeys(t *testing.T) {
	isolateDiscovery(t)
	initAt(t, t.TempDir())
	require.NoError(t, Set(UserDataKey, nil))

	got := GetAllFor(UserDataKey, EmailKey)

	assert.Equal(t, Settings{UserDataKey: nil}, got)
}

func TestSaveFailure(t *testing.T) {
	isolateDiscovery(t)

	t.Run("Set and Patch take effect only once saved", func(t *testing.T) {
		dir := t.TempDir()
		initAt(t, dir)
		require.NoError(t, Set(LocaleKey, "en-US"))
		before, err := os.ReadFile(filepath.Join(dir, settingsFileName))
		require.NoError(t, err)
		restore := moveAside(t, dir)

		assert.Error(t, Set(LocaleKey, "ru-RU"))
		assert.Error(t, Patch(Settings{LocaleKey: "ru-RU", CountryCodeKey: "RU"}))
		assert.Equal(t, "en-US", GetString(LocaleKey))
		assert.False(t, Exists(CountryCodeKey))
		restore()
		after, err := os.ReadFile(filepath.Join(dir, settingsFileName))
		require.NoError(t, err)
		assert.Equal(t, before, after, "the file keeps its previous contents")
		assert.Equal(t, Settings{LocaleKey: "ru-RU"}, Settings{LocaleKey: "ru-RU"}.Diff(GetAllFor(LocaleKey)),
			"a retry still sees the change")

		require.NoError(t, Patch(Settings{LocaleKey: "ru-RU", CountryCodeKey: "RU"}))
		assert.Equal(t, "ru-RU", GetString(LocaleKey))
		assert.Equal(t, "RU", GetString(CountryCodeKey))
	})

	t.Run("Apply takes effect even when the save fails", func(t *testing.T) {
		dir := t.TempDir()
		initAt(t, dir)
		defer moveAside(t, dir)()

		assert.Error(t, Apply(Settings{DataPathKey: "/data"}))

		assert.Equal(t, "/data", GetString(DataPathKey))
	})

	t.Run("Clear removes keys even when the save fails", func(t *testing.T) {
		dir := t.TempDir()
		initAt(t, dir)
		require.NoError(t, Patch(Settings{TokenKey: "secret", UserIDKey: 1}))
		defer moveAside(t, dir)()

		assert.Error(t, Clear(TokenKey, UserIDKey))

		assert.False(t, Exists(TokenKey))
		assert.False(t, Exists(UserIDKey))
	})
}

func TestPatchSkipsUnchangedWrites(t *testing.T) {
	isolateDiscovery(t)
	dir := t.TempDir()
	initAt(t, dir)
	path := filepath.Join(dir, settingsFileName)
	stat := func() os.FileInfo {
		t.Helper()
		info, err := os.Stat(path)
		require.NoError(t, err)
		return info
	}
	initial := stat()

	require.NoError(t, Patch(Settings{LocaleKey: "en-US", CountryCodeKey: "US"}))
	changed := stat()
	require.NoError(t, Patch(Settings{LocaleKey: "en-US"}))
	require.NoError(t, Set(CountryCodeKey, "US"))

	// atomicfile renames a new file into place, so a file with the same identity was never rewritten.
	assert.False(t, os.SameFile(initial, changed), "a real change is written")
	assert.True(t, os.SameFile(changed, stat()), "unchanged values are not rewritten")
}

func TestPatchRewritesAFileChangedElsewhere(t *testing.T) {
	isolateDiscovery(t)
	dir := t.TempDir()
	initAt(t, dir)
	require.NoError(t, Set(LocaleKey, "en-US"))
	path := filepath.Join(dir, settingsFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"locale": "ru-RU"}`), 0o644))

	require.NoError(t, Set(LocaleKey, "en-US"))

	require.NoError(t, Reload())
	assert.Equal(t, "en-US", GetString(LocaleKey))
}

func TestFailedInitKeepsChangesInMemory(t *testing.T) {
	isolateDiscovery(t)
	Reset()
	t.Cleanup(Reset)
	dir := t.TempDir()
	readOnlyDir(t, dir)
	require.Error(t, InitSettings(dir))

	require.NoError(t, Set(LocaleKey, "en-US"))

	assert.Equal(t, "en-US", GetString(LocaleKey))
}

func TestReload(t *testing.T) {
	isolateDiscovery(t)
	initSettingsAt := func(t *testing.T, contents string) string {
		t.Helper()
		Reset()
		t.Cleanup(Reset)
		dir := t.TempDir()
		path := filepath.Join(dir, settingsFileName)
		require.NoError(t, os.WriteFile(path, []byte(contents), 0644))
		require.NoError(t, InitSettings(dir))
		return path
	}

	t.Run("adopts values written by another process", func(t *testing.T) {
		path := initSettingsAt(t, `{"country_code": "US", "device_id": "old-device"}`)
		require.Equal(t, "US", GetString(CountryCodeKey))

		require.NoError(t, os.WriteFile(path, []byte(`{"country_code": "IR", "device_id": "new-device"}`), 0644))
		require.Equal(t, "US", GetString(CountryCodeKey), "cached value is served until a reload")

		require.NoError(t, Reload())
		assert.Equal(t, "IR", GetString(CountryCodeKey))
		assert.Equal(t, "new-device", GetString(DeviceIDKey))
	})

	t.Run("keeps this process's own writes", func(t *testing.T) {
		initSettingsAt(t, `{"country_code": "US"}`)
		require.NoError(t, Set(DeviceIDKey, "written-here"))

		require.NoError(t, Reload())
		assert.Equal(t, "written-here", GetString(DeviceIDKey), "Set persists before returning, so a reload reads it back")
	})

	t.Run("leaves settings intact when the file is unreadable", func(t *testing.T) {
		path := initSettingsAt(t, `{"country_code": "US"}`)
		require.NoError(t, os.WriteFile(path, []byte(`{invalid json}`), 0644))

		assert.Error(t, Reload())
		assert.Equal(t, "US", GetString(CountryCodeKey), "a failed reload must not wipe live settings")
	})

	t.Run("adopts deletions", func(t *testing.T) {
		path := initSettingsAt(t, `{"country_code": "US", "user_id": "user-1"}`)
		require.Equal(t, "user-1", GetString(UserIDKey))

		require.NoError(t, os.WriteFile(path, []byte(`{"country_code": "IR"}`), 0644))
		require.NoError(t, Reload())

		assert.Equal(t, "IR", GetString(CountryCodeKey), "changed values are adopted")
		assert.Empty(t, GetString(UserIDKey), "a removed key is dropped, not kept")
		assert.False(t, Exists(UserIDKey))
	})

	t.Run("missing file is not an error", func(t *testing.T) {
		path := initSettingsAt(t, `{"country_code": "US"}`)
		require.NoError(t, os.Remove(path))

		assert.NoError(t, Reload())
		assert.Equal(t, "US", GetString(CountryCodeKey))
	})

	t.Run("errors when uninitialized", func(t *testing.T) {
		Reset()
		t.Cleanup(Reset)
		assert.Error(t, Reload())
	})
}

func TestReloadRacesNothing(t *testing.T) {
	isolateDiscovery(t)
	Reset()
	t.Cleanup(Reset)
	dir := t.TempDir()
	path := filepath.Join(dir, settingsFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"country_code": "US"}`), 0644))
	require.NoError(t, InitSettings(dir))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = GetString(CountryCodeKey)
					_ = GetBool(SplitTunnelKey)
				}
			}
		}()
	}
	for range 50 {
		require.NoError(t, Reload())
	}
	close(stop)
	wg.Wait()
}
