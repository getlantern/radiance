package legacymigration

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/getlantern/radiance/common/atomicfile"
)

// WriteSettings installs an accepted identity without overwriting existing settings.
// Completed migrations preserve later account changes.
func WriteSettings(dataDir string, record *Record) error {
	if err := validateRecord(record); err != nil || record.Request == nil {
		return errors.New("no accepted migration identity")
	}
	path := filepath.Join(dataDir, "settings.json")
	existing, err := os.ReadFile(path)
	if err == nil {
		var current struct {
			UserID     int64  `json:"user_id"`
			Token      string `json:"token"`
			DeviceID   string `json:"device_id"`
			Migration  string `json:"legacy_migration_id"`
			RequestSHA string `json:"legacy_migration_sha256"`
		}
		if json.Unmarshal(existing, &current) != nil || current.Migration != record.MigrationID || current.RequestSHA != record.RequestSHA256 {
			return ErrConflict
		}
		if record.Status == "completed" {
			return nil
		}
		if current.UserID != record.Request.UserID || current.Token != record.Request.Token || current.DeviceID != record.Request.DeviceID {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if record.Status == "completed" {
		return ErrConflict
	}
	request := record.Request
	data := map[string]any{
		"user_id": request.UserID, "token": request.Token, "device_id": request.DeviceID,
		"user_level": record.Receipt.UserLevel, "user_data": record.UserData,
		"locale": request.Locale, "telemetry_enabled": request.AutoReport,
		"smart_routing": !request.ProxyAll,
		// The destination must stay disconnected while the legacy app is active.
		"auto_connect":        false,
		"legacy_migration_id": record.MigrationID, "legacy_migration_sha256": record.RequestSHA256,
		"legacy_auto_launch": request.AutoLaunch,
	}
	var user struct {
		LegacyUserData struct {
			Email string `json:"email"`
		} `json:"legacyUserData"`
	}
	if err := json.Unmarshal(record.UserData, &user); err != nil {
		return err
	}
	data["email"] = user.LegacyUserData.Email
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, raw, 0600)
}
