package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigratedIdentityPreservesInt64AcrossRestart(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	dir := t.TempDir()
	data := []byte(`{"legacy_migration_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","user_id":9007199254740993,"user_data":{"legacyID":9007199254740993,"legacyUserData":{"userId":9007199254740993}}}`)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := InitSettings(dir); err != nil {
			t.Fatal(err)
		}
		if GetInt64(UserIDKey) != 9007199254740993 {
			t.Fatal("rounded adopted user ID")
		}
		var user struct{ LegacyID int64 }
		if err := GetStruct(UserDataKey, &user); err != nil || user.LegacyID != 9007199254740993 {
			t.Fatalf("rounded cached user ID: %+v %v", user, err)
		}
		if err := Set(LocaleKey, "fa-IR"); err != nil {
			t.Fatal(err)
		}
		Reset()
	}
}
