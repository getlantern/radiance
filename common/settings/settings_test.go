package settings

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/getlantern/radiance/common/env"
)

func TestInitSettings(t *testing.T) {
	t.Run("existing valid config file", func(t *testing.T) {
		tempDir := t.TempDir()
		path := filepath.Join(tempDir, settingsFileName)
		content := []byte(`{"locale": "en-US", "country_code": "US"}`)
		require.NoError(t, os.WriteFile(path, content, 0644), "failed to create test config file")

		require.NoError(t, InitSettings(tempDir), "failed to initialize settings")
		assert.Equal(t, "en-US", Get(LocaleKey))
		assert.Equal(t, "US", Get(CountryCodeKey))
	})

	t.Run("invalid config file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), settingsFileName)
		content := []byte(`{invalid json}`)
		require.NoError(t, os.WriteFile(path, content, 0644), "failed to create test config file")
		require.Error(t, loadSettings(path), "expected error for invalid config file")
	})
}

func TestMigrateLegacySettingsIfNeeded(t *testing.T) {
	// Redirect the OS-specific pre-9.x YAML lookup to nowhere by
	// default so individual tests don't pick up the host machine's
	// actual ~/Library/Application Support/Lantern/settings.yaml or
	// equivalent. Sub-tests that exercise the YAML path opt in by
	// pointing the function at their tempDir.
	prevYAMLPath := legacyYAMLPathFn
	legacyYAMLPathFn = func(string) (string, string) { return "", "" }
	t.Cleanup(func() { legacyYAMLPathFn = prevYAMLPath })

	writeNested := func(t *testing.T, dir string, contents []byte) {
		t.Helper()
		nd := filepath.Join(dir, "data")
		require.NoError(t, os.MkdirAll(nd, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(nd, settingsFileName), contents, 0o644))
	}
	writeLegacy := func(t *testing.T, dir string, contents []byte) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, legacySettingsFileName), contents, 0o644))
	}

	t.Run("v9.0.x local.json recovered when canonical is missing (Derek's failing case)", func(t *testing.T) {
		// User upgraded from v9.0.x straight to the fixed build. v9.0.x wrote
		// to <dataDir>/local.json; canonical settings.json doesn't exist;
		// no v9.1.x nested file. The fix must read local.json so Pro survives.
		tempDir := t.TempDir()
		want := []byte(`{"user_id": 3580849, "user_level": "pro", "token": "abc"}`)
		writeLegacy(t, tempDir, want)

		canonical := filepath.Join(tempDir, settingsFileName)
		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, want, got, "v9.0.x local.json should be migrated to canonical")
	})

	t.Run("v9.1.x nested file recovered when canonical is missing", func(t *testing.T) {
		tempDir := t.TempDir()
		want := []byte(`{"user_id": 135809562, "user_level": "pro", "device_id": "abc"}`)
		writeNested(t, tempDir, want)

		canonical := filepath.Join(tempDir, settingsFileName)
		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, want, got, "v9.1.x nested file should be migrated to canonical")
	})

	t.Run("v9.0.x local.json wins over v9.1.x expired nested", func(t *testing.T) {
		// Upgrade chain v9.0.x → v9.1.x → fix: legacy has pro, nested has
		// expired (because v9.1.x lost the user_id). Migration must pick
		// legacy so Pro survives.
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		legacyPro := []byte(`{"user_id": 1, "user_level": "pro"}`)
		writeLegacy(t, tempDir, legacyPro)
		writeNested(t, tempDir, []byte(`{"user_id": 999, "user_level": "expired"}`))

		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, legacyPro, got, "legacy local.json with pro should beat nested expired")
	})

	t.Run("canonical-pro wins over nested-expired", func(t *testing.T) {
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		canonicalPro := []byte(`{"user_id": 1, "user_level": "pro"}`)
		require.NoError(t, os.WriteFile(canonical, canonicalPro, 0o644))
		writeNested(t, tempDir, []byte(`{"user_id": 999, "user_level": "expired"}`))

		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, canonicalPro, got, "canonical-pro should survive")
	})

	t.Run("nested-pro wins over canonical-expired and legacy-expired", func(t *testing.T) {
		// e.g., user paid via Shepherd while on v9.1.x, so the nested file
		// legitimately holds pro state.
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		require.NoError(t, os.WriteFile(canonical, []byte(`{"user_id": 1, "user_level": "expired"}`), 0o644))
		writeLegacy(t, tempDir, []byte(`{"user_id": 1, "user_level": "expired"}`))
		nestedPro := []byte(`{"user_id": 2, "user_level": "pro"}`)
		writeNested(t, tempDir, nestedPro)

		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, nestedPro, got, "nested-pro should beat both canonical and legacy when only it has pro")
	})

	t.Run("all-pro: canonical wins (most recent deliberate state)", func(t *testing.T) {
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		canonicalContents := []byte(`{"user_id": 1, "user_level": "pro"}`)
		require.NoError(t, os.WriteFile(canonical, canonicalContents, 0o644))
		writeLegacy(t, tempDir, []byte(`{"user_id": 2, "user_level": "pro"}`))
		writeNested(t, tempDir, []byte(`{"user_id": 3, "user_level": "pro"}`))

		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, canonicalContents, got, "canonical preferred when all have pro")
	})

	t.Run("none have pro: legacy wins over nested when canonical missing", func(t *testing.T) {
		// User identifiers must survive even when Pro state is non-pro,
		// to keep the device registration intact server-side.
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		legacyContents := []byte(`{"user_id": 1, "user_level": "free", "token": "abc"}`)
		writeLegacy(t, tempDir, legacyContents)
		writeNested(t, tempDir, []byte(`{"user_id": 2, "user_level": "free", "token": "xyz"}`))

		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, legacyContents, got, "legacy preferred over nested when canonical missing and neither has pro")
	})

	t.Run("nothing on disk is a no-op", func(t *testing.T) {
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)

		migrateLegacySettingsIfNeeded(tempDir, canonical)

		_, err := os.Stat(canonical)
		assert.True(t, os.IsNotExist(err), "no migration when no source files exist")
	})

	t.Run("pre-9.x desktop YAML recovered when no JSON candidates exist", func(t *testing.T) {
		// Redirect the YAML lookup at a tempDir-local file so the test
		// is portable across OSes.
		tempDir := t.TempDir()
		yamlPath := filepath.Join(tempDir, "fake-pre9x-settings.yaml")
		require.NoError(t, os.WriteFile(yamlPath, []byte(`userID: 3580849
deviceID: legacy-device-id
userPro: true
userToken: legacy-token
emailAddress: derek@example.com
`), 0o644))
		legacyYAMLPathFn = func(string) (string, string) { return yamlPath, "desktop" }
		t.Cleanup(func() { legacyYAMLPathFn = func(string) (string, string) { return "", "" } })

		canonical := filepath.Join(tempDir, settingsFileName)
		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		gotStr := string(got)
		assert.Contains(t, gotStr, `"user_id":3580849`)
		assert.Contains(t, gotStr, `"device_id":"legacy-device-id"`)
		assert.Contains(t, gotStr, `"user_level":"pro"`)
		assert.Contains(t, gotStr, `"token":"legacy-token"`)
		assert.Contains(t, gotStr, `"email":"derek@example.com"`)
	})

	t.Run("v9.0.x local.json beats pre-9.x YAML", func(t *testing.T) {
		// Both exist with pro state. local.json is the higher-priority
		// (more recent) source, so it should win.
		tempDir := t.TempDir()
		yamlPath := filepath.Join(tempDir, "fake-pre9x-settings.yaml")
		require.NoError(t, os.WriteFile(yamlPath, []byte(`userID: 1
userPro: true
userToken: legacy-token
`), 0o644))
		legacyYAMLPathFn = func(string) (string, string) { return yamlPath, "desktop" }
		t.Cleanup(func() { legacyYAMLPathFn = func(string) (string, string) { return "", "" } })

		writeLegacy(t, tempDir, []byte(`{"user_id": 2, "user_level": "pro", "token": "v9.0-token"}`))

		canonical := filepath.Join(tempDir, settingsFileName)
		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Contains(t, string(got), `"user_id": 2`,
			"v9.0.x local.json should win over pre-9.x YAML when both have pro")
		assert.Contains(t, string(got), `"v9.0-token"`)
	})

	t.Run("pre-9.x YAML beats v9.1.x bugged nested file", func(t *testing.T) {
		// Pre-9.x has pro; v9.1.x nested has expired (the bugged case).
		// Pre-9.x must win.
		tempDir := t.TempDir()
		yamlPath := filepath.Join(tempDir, "fake-pre9x-settings.yaml")
		require.NoError(t, os.WriteFile(yamlPath, []byte(`userID: 1
userPro: true
userToken: legacy-token
`), 0o644))
		legacyYAMLPathFn = func(string) (string, string) { return yamlPath, "desktop" }
		t.Cleanup(func() { legacyYAMLPathFn = func(string) (string, string) { return "", "" } })

		writeNested(t, tempDir, []byte(`{"user_id": 999, "user_level": "expired"}`))

		canonical := filepath.Join(tempDir, settingsFileName)
		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Contains(t, string(got), `"user_level":"pro"`,
			"pre-9.x YAML with pro should win over v9.1.x nested expired")
		assert.Contains(t, string(got), `"legacy-token"`)
	})

	t.Run("iOS userconfig.yaml recovered when canonical is missing", func(t *testing.T) {
		// On iOS the legacy YAML is sandbox-relative — it lives next to
		// where settings.json now lives, so legacyYAMLCandidate reads
		// from fileDir directly and we can exercise it from a test
		// without monkeypatching $HOME or $APPDATA. (Desktop legacy
		// paths are covered via translateLegacyYAML's unit tests, which
		// don't depend on the OS-specific path resolution.)
		if runtime.GOOS != "ios" {
			t.Skip("iOS-only path: legacy YAML elsewhere is OS-specific")
		}
		tempDir := t.TempDir()
		yamlPath := filepath.Join(tempDir, "userconfig.yaml")
		require.NoError(t, os.WriteFile(yamlPath, []byte(`UserID: 7777
DeviceID: ios-device
Token: tok
`), 0o644))
		canonical := filepath.Join(tempDir, settingsFileName)
		migrateLegacySettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Contains(t, string(got), `"user_id":7777`)
		assert.Contains(t, string(got), `"device_id":"ios-device"`)
	})

	t.Run("unreadable canonical (non-ENOENT) skips migration", func(t *testing.T) {
		// Permission error on the canonical path: don't fall through and
		// overwrite a file we couldn't read. unix only — windows handles
		// permissions differently and chmod wouldn't reproduce this.
		if runtime.GOOS == "windows" {
			t.Skip("permission semantics differ on windows")
		}
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		require.NoError(t, os.WriteFile(canonical, []byte(`{"user_level": "expired"}`), 0o644))
		// Make the file unreadable.
		require.NoError(t, os.Chmod(canonical, 0o000))
		t.Cleanup(func() { _ = os.Chmod(canonical, 0o644) })
		// Stage a legacy-pro candidate that would otherwise win.
		writeLegacy(t, tempDir, []byte(`{"user_id": 1, "user_level": "pro"}`))

		migrateLegacySettingsIfNeeded(tempDir, canonical)

		// Restore readability and confirm the canonical contents are
		// unchanged (still the expired body, not the legacy-pro body).
		require.NoError(t, os.Chmod(canonical, 0o644))
		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, `{"user_level": "expired"}`, string(got),
			"canonical with non-ENOENT read error should be left alone")
	})
}
