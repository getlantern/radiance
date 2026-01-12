package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitSettings(t *testing.T) {
	t.Run("first run - no config file exists", func(t *testing.T) {
		tempDir := t.TempDir()
		err := initialize(tempDir)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify default locale was set
		locale := Get(LocaleKey)
		if locale != "fa-IR" {
			t.Errorf("expected default locale 'fa-IR', got %s", locale)
		}

		// Verify config file was created
		configPath := filepath.Join(tempDir, "local.json")
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			t.Error("expected config file to be created")
		}
	})

	t.Run("existing valid config file", func(t *testing.T) {
		// Create a temporary directory
		tempDir := t.TempDir()

		// Create a valid config file
		configPath := filepath.Join(tempDir, "local.json")
		configContent := []byte(`{"locale": "en-US", "country_code": "US"}`)
		if err := os.WriteFile(configPath, configContent, 0644); err != nil {
			t.Fatalf("failed to create test config file: %v", err)
		}

		err := initialize(tempDir)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify config was loaded
		locale := Get(LocaleKey)
		if locale != "en-US" {
			t.Errorf("expected locale 'en-US', got %s", locale)
		}

		countryCode := Get(CountryCodeKey)
		if countryCode != "US" {
			t.Errorf("expected country_code 'US', got %s", countryCode)
		}
	})

	t.Run("invalid config file", func(t *testing.T) {
		// Create a temporary directory
		tempDir := t.TempDir()

		// Create an invalid config file
		configPath := filepath.Join(tempDir, "local.json")
		configContent := []byte(`{invalid json}`)
		if err := os.WriteFile(configPath, configContent, 0644); err != nil {
			t.Fatalf("failed to create test config file: %v", err)
		}

		err := initialize(tempDir)
		if err == nil {
			t.Fatal("expected error for invalid config file, got nil")
		}
	})

	t.Run("non-existent directory", func(t *testing.T) {
		// Use a non-existent directory
		nonExistentDir := filepath.Join(os.TempDir(), "non-existent-dir-123456789")

		err := initialize(nonExistentDir)
		if err != nil {
			t.Fatalf("expected no error for non-existent directory (first run), got %v", err)
		}
	})
}

func TestSetStruct(t *testing.T) {
	tempDir := t.TempDir()
	err := initialize(tempDir)
	if err != nil {
		t.Fatalf("expected no error initializing settings, got %v", err)
	}

	err = Set("testStruct", struct {
		Field1 string
		Field2 int
	}{
		Field1: "value1",
		Field2: 42,
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	var result struct {
		Field1 string
		Field2 int
	}
	err = GetStruct("testStruct", &result)
	if err != nil {
		t.Fatalf("expected no error retrieving struct, got %v", err)
	}

	if result.Field1 != "value1" || result.Field2 != 42 {
		t.Errorf("expected struct {Field1: 'value1', Field2: 42}, got %+v", result)
	}

	// Reset koanf state
	Reset()
	result.Field1 = ""
	result.Field2 = 0

	// At first, the struct should not be present.
	err = GetStruct("testStruct", &result)
	if err != nil {
		t.Fatalf("expected no error retrieving struct, got %v", err)
	}

	if result.Field1 != "" || result.Field2 != 0 {
		t.Errorf("expected struct {Field1: '', Field2: 0}, got %+v", result)
	}

	err = initialize(tempDir)
	if err != nil {
		t.Fatalf("expected no error re-initializing settings, got %v", err)
	}

	var result2 struct {
		Field1 string
		Field2 int
	}
	err = GetStruct("testStruct", &result2)
	if err != nil {
		t.Fatalf("expected no error retrieving struct after re-init, got %v", err)
	}

	if result2.Field1 != "value1" || result2.Field2 != 42 {
		t.Errorf("expected struct {Field1: 'value1', Field2: 42} after re-init, got %+v", result2)
	}
}

func TestStructSlicePersistence(t *testing.T) {
	tempDir := t.TempDir()
	err := initialize(tempDir)
	if err != nil {
		t.Fatalf("expected no error initializing settings, got %v", err)
	}

	type Item struct {
		Name  string
		Value int
	}

	items := []Item{
		{Name: "item1", Value: 1},
		{Name: "item2", Value: 2},
	}

	err = Set("itemList", items)
	if err != nil {
		t.Fatalf("expected no error setting struct slice, got %v", err)
	}

	var retrievedItems []Item
	err = GetStruct("itemList", &retrievedItems)
	if err != nil {
		t.Fatalf("expected no error retrieving struct slice, got %v", err)
	}

	if len(retrievedItems) != 2 || retrievedItems[0].Name != "item1" || retrievedItems[1].Value != 2 {
		t.Errorf("retrieved struct slice does not match expected values: %+v", retrievedItems)
	}

	err = initialize(tempDir)
	if err != nil {
		t.Fatalf("expected no error re-initializing settings, got %v", err)
	}

	var retrievedItems2 []Item
	err = GetStruct("itemList", &retrievedItems2)
	if err != nil {
		t.Fatalf("expected no error retrieving struct slice after re-init, got %v", err)
	}

	if len(retrievedItems2) != 2 || retrievedItems2[0].Name != "item1" || retrievedItems2[1].Value != 2 {
		t.Errorf("retrieved struct slice after re-init does not match expected values: %+v", retrievedItems2)
	}
}
