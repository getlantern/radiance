package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func TestInitSettings(t *testing.T) {
	t.Run("first run - no config file exists", func(t *testing.T) {
		// Create a temporary directory
		//tempDir := "/Users/Shared/temp" //t.TempDir()
		tempDir := t.TempDir()
		// Ensure the directory exists
		if err := os.MkdirAll(tempDir, 0755); err != nil {
			t.Fatalf("failed to create temp directory: %v", err)
		}

		// Reset viper state
		viper.Reset()

		err := InitSettings(tempDir)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify default locale was set
		locale := viper.GetString(LocaleKey)
		if locale != "fa-IR" {
			t.Errorf("expected default locale 'fa-IR', got %s", locale)
		}

		// Verify config file was created
		configPath := filepath.Join(tempDir, "local.json")
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			t.Error("expected config file to be created")
		}

		viper.Reset()

		viper.SetConfigName("local.json")
		viper.AddConfigPath(tempDir)
		viper.SetConfigType("json")
		if err := viper.ReadInConfig(); err != nil {
			t.Fatalf("failed to read config file: %v", err)
		}

		// Verify default locale persists after re-reading config
		locale = viper.GetString(LocaleKey)
		if locale != "fa-IR" {
			t.Errorf("expected default locale 'fa-IR' after re-reading config, got %s", locale)
		}
	})

	t.Run("existing valid config file", func(t *testing.T) {
		// Create a temporary directory
		tempDir := t.TempDir()
		// Ensure the directory exists
		if err := os.MkdirAll(tempDir, 0755); err != nil {
			t.Fatalf("failed to create temp directory: %v", err)
		}

		// Create a valid config file
		configPath := filepath.Join(tempDir, "local.json")
		configContent := []byte(`{"locale": "en-US", "country_code": "US"}`)
		if err := os.WriteFile(configPath, configContent, 0644); err != nil {
			t.Fatalf("failed to create test config file: %v", err)
		}

		// Reset viper state
		viper.Reset()

		err := InitSettings(tempDir)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify config was loaded
		locale := viper.GetString(LocaleKey)
		if locale != "en-US" {
			t.Errorf("expected locale 'en-US', got %s", locale)
		}

		countryCode := viper.GetString(CountryCodeKey)
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

		// Reset viper state
		viper.Reset()

		err := InitSettings(tempDir)
		if err == nil {
			t.Fatal("expected error for invalid config file, got nil")
		}
	})

	t.Run("non-existent directory", func(t *testing.T) {
		// Reset viper state
		viper.Reset()

		// Use a non-existent directory
		nonExistentDir := filepath.Join(os.TempDir(), "non-existent-dir-123456789")

		err := InitSettings(nonExistentDir)
		if err != nil {
			t.Fatalf("expected no error for non-existent directory (first run), got %v", err)
		}
	})
}
