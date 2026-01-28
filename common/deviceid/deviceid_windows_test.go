//go:build windows
// +build windows

package deviceid

import (
	"testing"

	"github.com/google/uuid"
	"golang.org/x/sys/windows/registry"
)

func TestGet(t *testing.T) {
	// Clean up before and after test
	cleanupRegistry := func() {
		key, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
		if err == nil {
			key.DeleteValue("deviceid")
			key.Close()
		}
	}

	t.Run("creates new deviceID when not exists", func(t *testing.T) {
		cleanupRegistry()
		defer cleanupRegistry()

		deviceID := Get()
		if deviceID == "" {
			t.Fatal("expected non-empty deviceID")
		}

		// Verify it's a valid UUID
		if _, err := uuid.Parse(deviceID); err != nil {
			t.Errorf("expected valid UUID, got error: %v", err)
		}
	})

	t.Run("returns existing deviceID", func(t *testing.T) {
		cleanupRegistry()
		defer cleanupRegistry()

		// First call creates the ID
		firstID := Get()

		// Second call should return the same ID
		secondID := Get()

		if firstID != secondID {
			t.Errorf("expected same deviceID, got %s and %s", firstID, secondID)
		}
	})

	t.Run("persists deviceID in registry", func(t *testing.T) {
		cleanupRegistry()
		defer cleanupRegistry()

		deviceID := Get()

		// Read directly from registry
		key, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
		if err != nil {
			t.Fatalf("failed to open registry key: %v", err)
		}
		defer key.Close()

		storedID, _, err := key.GetStringValue("deviceid")
		if err != nil {
			t.Fatalf("failed to read deviceid from registry: %v", err)
		}

		if storedID != deviceID {
			t.Errorf("expected deviceID %s in registry, got %s", deviceID, storedID)
		}
	})
}
