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
	writeNested := func(t *testing.T, dir string, contents []byte) {
		t.Helper()
		nd := filepath.Join(dir, "data")
		require.NoError(t, os.MkdirAll(nd, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(nd, settingsFileName), contents, 0o644))
	}

	t.Run("nested file recovered when canonical is missing", func(t *testing.T) {
		tempDir := t.TempDir()
		want := []byte(`{"user_id": 135809562, "user_level": "pro", "device_id": "abc"}`)
		writeNested(t, tempDir, want)

		canonical := filepath.Join(tempDir, settingsFileName)
		migrateV91xSettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, want, got, "migrated content should match nested file")
	})

	t.Run("canonical-pro wins over nested-expired (the typical broken upgrade)", func(t *testing.T) {
		// v9.0.x wrote canonical with pro; v9.1.x wrote nested with expired
		// because it lost the user_id. The fixed client must keep canonical.
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		canonicalPro := []byte(`{"user_id": 1, "user_level": "pro"}`)
		require.NoError(t, os.WriteFile(canonical, canonicalPro, 0o644))
		writeNested(t, tempDir, []byte(`{"user_id": 999, "user_level": "expired"}`))

		migrateV91xSettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, canonicalPro, got, "canonical-pro should survive when nested is expired")
	})

	t.Run("nested-pro wins over canonical-expired (rare inverse case)", func(t *testing.T) {
		// e.g., user paid via Shepherd during the v9.1.x window so the
		// nested file legitimately has pro while canonical is stale.
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		require.NoError(t, os.WriteFile(canonical, []byte(`{"user_id": 1, "user_level": "expired"}`), 0o644))
		nestedPro := []byte(`{"user_id": 2, "user_level": "pro"}`)
		writeNested(t, tempDir, nestedPro)

		migrateV91xSettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, nestedPro, got, "nested-pro should overwrite canonical-expired")
	})

	t.Run("both pro: canonical wins (older deliberate state)", func(t *testing.T) {
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		canonicalContents := []byte(`{"user_id": 1, "user_level": "pro"}`)
		require.NoError(t, os.WriteFile(canonical, canonicalContents, 0o644))
		writeNested(t, tempDir, []byte(`{"user_id": 2, "user_level": "pro"}`))

		migrateV91xSettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, canonicalContents, got, "canonical preferred when both pro")
	})

	t.Run("neither pro: canonical wins", func(t *testing.T) {
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)
		canonicalContents := []byte(`{"user_id": 1, "user_level": "free"}`)
		require.NoError(t, os.WriteFile(canonical, canonicalContents, 0o644))
		writeNested(t, tempDir, []byte(`{"user_id": 2, "user_level": "free"}`))

		migrateV91xSettingsIfNeeded(tempDir, canonical)

		got, err := os.ReadFile(canonical)
		require.NoError(t, err)
		assert.Equal(t, canonicalContents, got, "canonical preferred when neither has pro")
	})

	t.Run("no nested file is a no-op", func(t *testing.T) {
		tempDir := t.TempDir()
		canonical := filepath.Join(tempDir, settingsFileName)

		migrateV91xSettingsIfNeeded(tempDir, canonical)

		_, err := os.Stat(canonical)
		assert.True(t, os.IsNotExist(err), "no migration when no nested file exists")
	})
}
