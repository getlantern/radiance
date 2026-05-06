package settings

import (
	"os"
	"path/filepath"
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

func TestMigrateV91xSettingsIfNeeded(t *testing.T) {
	t.Run("nested file recovered when canonical is missing", func(t *testing.T) {
		tempDir := t.TempDir()
		nestedDir := filepath.Join(tempDir, "data")
		require.NoError(t, os.MkdirAll(nestedDir, 0o755))
		nestedPath := filepath.Join(nestedDir, settingsFileName)
		want := []byte(`{"user_id": 135809562, "user_level": "pro", "device_id": "abc"}`)
		require.NoError(t, os.WriteFile(nestedPath, want, 0o644))

		canonical := filepath.Join(tempDir, settingsFileName)
		migrateV91xSettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err, "canonical settings.json should exist after migration")
		assert.Equal(t, want, got, "migrated content should match nested file")
	})

	t.Run("canonical present takes precedence; nested ignored", func(t *testing.T) {
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		canonicalContents := []byte(`{"user_id": 1, "user_level": "pro"}`)
		require.NoError(t, os.WriteFile(canonical, canonicalContents, 0o644))

		nestedDir := filepath.Join(tempDir, "data")
		require.NoError(t, os.MkdirAll(nestedDir, 0o755))
		nestedPath := filepath.Join(nestedDir, settingsFileName)
		require.NoError(t, os.WriteFile(nestedPath, []byte(`{"user_id": 999}`), 0o644))

		migrateV91xSettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, canonicalContents, got, "canonical should be unchanged when it already exists")
	})

	t.Run("no nested file is a no-op", func(t *testing.T) {
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)

		migrateV91xSettingsIfNeeded(tempDir, canonical)

		_, err := os.Stat(canonical)
		assert.True(t, os.IsNotExist(err), "no migration when no nested file exists")
	})
}
