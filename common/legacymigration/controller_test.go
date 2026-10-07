package legacymigration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type memoryStore struct {
	record *Record
	fail   bool
	saves  int
}

func (s *memoryStore) Load() (*Record, error) {
	raw, _ := json.Marshal(s.record)
	var copy *Record
	_ = json.Unmarshal(raw, &copy)
	return copy, nil
}

func (s *memoryStore) Save(record *Record) error {
	if s.fail {
		return errors.New("disk unavailable")
	}
	raw, _ := json.Marshal(record)
	_ = json.Unmarshal(raw, &s.record)
	s.saves++
	return nil
}

func fixture(t *testing.T) (*Controller, *memoryStore, []byte) {
	t.Helper()
	store := &memoryStore{record: &Record{Version: 1, MigrationID: strings.Repeat("a", 32),
		SourceSID: "S-1-5-21-123", SourceDirectory: `C:\Users\Original\AppData\Roaming\Lantern`, Status: "pending"}}
	controller, err := NewController(store, func(context.Context, Request) (json.RawMessage, string, error) {
		return json.RawMessage(`{"legacyID":42,"legacyToken":"secret","legacyUserData":{"userId":42,"userLevel":"pro"}}`), "pro", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Version: 1, MigrationID: store.record.MigrationID, SourceDirectory: store.record.SourceDirectory,
		UserID: 42, Token: "secret", DeviceID: "old-device", Locale: "fa-IR", AutoReport: true, ProxyAll: true, AutoLaunch: true}
	raw, _ := json.Marshal(request)
	return controller, store, raw
}

func TestAdoptionAuthorizesActualSIDAndExactReplay(t *testing.T) {
	controller, store, raw := fixture(t)
	if _, err := controller.Adopt(context.Background(), "S-1-5-21-other", raw); !errors.Is(err, ErrUnauthorized) || store.saves != 0 {
		t.Fatalf("other user adopted: %v", err)
	}
	receipt, err := controller.Adopt(context.Background(), store.record.SourceSID, raw)
	if err != nil || receipt.Status != "adopted" || receipt.UserLevel != "pro" {
		t.Fatalf("adopt: %+v %v", receipt, err)
	}
	if _, err := controller.Adopt(context.Background(), store.record.SourceSID, append(raw, ' ')); !errors.Is(err, ErrConflict) {
		t.Fatalf("different bytes replayed: %v", err)
	}
	if got, err := controller.Adopt(context.Background(), store.record.SourceSID, raw); err != nil || got != receipt || store.saves != 1 {
		t.Fatalf("exact replay changed record: %+v %v", got, err)
	}
	encoded, _ := json.Marshal(receipt)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), `"token"`) {
		t.Fatal("receipt contains credentials")
	}
}

func TestFailedVerificationOrSaveLeavesPending(t *testing.T) {
	for _, mode := range []string{"verify", "save"} {
		t.Run(mode, func(t *testing.T) {
			controller, store, raw := fixture(t)
			if mode == "verify" {
				controller.verify = func(context.Context, Request) (json.RawMessage, string, error) {
					return nil, "", errors.New("secret upstream body")
				}
			} else {
				store.fail = true
			}
			_, err := controller.Adopt(context.Background(), store.record.SourceSID, raw)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe failure: %v", err)
			}
			if store.record.Request != nil || controller.record.Request != nil || store.saves != 0 {
				t.Fatal("failed request persisted identity")
			}
		})
	}
}

func TestMalformedRequestsNeverReachVerification(t *testing.T) {
	for _, change := range []func([]byte) []byte{
		func(raw []byte) []byte { return []byte(strings.Replace(string(raw), `"version":1`, `"version":2`, 1)) },
		func(raw []byte) []byte { return []byte(strings.Replace(string(raw), `"user_id":42`, `"user_id":0`, 1)) },
		func(raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `"token":"secret"`, `"token":""`, 1))
		},
		func(raw []byte) []byte { return append(raw, []byte(`{}`)...) },
		func(raw []byte) []byte { return append([]byte(`{"unknown":true,`), raw[1:]...) },
		func(raw []byte) []byte { return []byte(strings.Repeat("x", maxRequestBytes+1)) },
	} {
		controller, store, raw := fixture(t)
		controller.verify = func(context.Context, Request) (json.RawMessage, string, error) {
			t.Fatal("verified invalid request")
			return nil, "", nil
		}
		if _, err := controller.Adopt(context.Background(), store.record.SourceSID, change(raw)); err == nil {
			t.Fatal("accepted malformed request")
		}
	}
}

func TestReadyIsLiveAndMustBeReestablishedAfterRestart(t *testing.T) {
	controller, store, raw := fixture(t)
	if _, err := controller.Adopt(context.Background(), store.record.SourceSID, raw); err != nil {
		t.Fatal(err)
	}
	userID := int64(42)
	if err := controller.MarkReady(func() State {
		return State{UserID: userID, Token: "secret", DeviceID: "old-device", Locale: "fa-IR", UserLevel: "pro", AutoReport: true, ProxyAll: true, AutoLaunch: true, Healthy: true}
	}); err != nil {
		t.Fatal(err)
	}
	if store.record.Status != "completed" {
		t.Fatal("completion not durable")
	}
	got, _ := controller.Status(store.record.SourceSID)
	if got.Status != "ready" {
		t.Fatal("backend not ready")
	}
	userID = 99
	got, _ = controller.Status(store.record.SourceSID)
	if got.Status == "ready" {
		t.Fatal("stale readiness after account changed")
	}
	restarted, err := NewController(store, controller.verify)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = restarted.Status(store.record.SourceSID)
	if got.Status == "ready" {
		t.Fatal("persisted receipt falsely proves live readiness")
	}
}

func TestConcurrentReplayCommitsOnlyOnce(t *testing.T) {
	controller, store, raw := fixture(t)
	sid := store.record.SourceSID
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, err := controller.Adopt(context.Background(), sid, raw); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if store.saves != 1 {
		t.Fatalf("committed %d times", store.saves)
	}
}

func TestReadyRejectsLostCredentialsPreferencesAndBackend(t *testing.T) {
	for _, change := range []func(*State){
		func(s *State) { s.Token = "wrong" }, func(s *State) { s.DeviceID = "service-device" },
		func(s *State) { s.Locale = "en-US" }, func(s *State) { s.AutoReport = false },
		func(s *State) { s.ProxyAll = false }, func(s *State) { s.AutoLaunch = false },
		func(s *State) { s.UserLevel = "free" }, func(s *State) { s.Healthy = false },
	} {
		controller, store, raw := fixture(t)
		if _, err := controller.Adopt(context.Background(), store.record.SourceSID, raw); err != nil {
			t.Fatal(err)
		}
		state := State{UserID: 42, Token: "secret", DeviceID: "old-device", Locale: "fa-IR", UserLevel: "pro", AutoReport: true, ProxyAll: true, AutoLaunch: true, Healthy: true}
		change(&state)
		if err := controller.MarkReady(func() State { return state }); !errors.Is(err, ErrConflict) {
			t.Fatalf("ready despite state mismatch: %v", err)
		}
		if controller.Completed() {
			t.Fatal("bad state committed completion")
		}
	}
}

func TestRollbackRequiresOwnerAndBindingAndConfirmedDisconnect(t *testing.T) {
	controller, store, raw := fixture(t)
	receipt, err := controller.Adopt(context.Background(), store.record.SourceSID, raw)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	controller.SetRollback(func(context.Context) error { calls++; return nil })
	if _, err := controller.Rollback(context.Background(), "other", receipt.MigrationID, receipt.RequestSHA256); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := controller.Rollback(context.Background(), receipt.SourceSID, receipt.MigrationID, "wrong"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("unauthorized rollback reached backend")
	}
	if _, err := controller.Rollback(context.Background(), receipt.SourceSID, receipt.MigrationID, receipt.RequestSHA256); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || store.saves != 1 {
		t.Fatal("rollback modified persisted identity")
	}
	controller.SetRollback(func(context.Context) error { return errors.New("disconnect failed") })
	if _, err := controller.Rollback(context.Background(), receipt.SourceSID, receipt.MigrationID, receipt.RequestSHA256); err == nil {
		t.Fatal("unconfirmed disconnect succeeded")
	}
}

func TestSettingsConflictRecoveryAndCompletedAccountChange(t *testing.T) {
	controller, store, raw := fixture(t)
	if _, err := controller.Adopt(context.Background(), store.record.SourceSID, raw); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"user_id":99,"token":"modern"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(dir, store.record); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrote existing account: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(dir, store.record); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(dir, store.record); err != nil {
		t.Fatalf("crash recovery failed: %v", err)
	}
	contents, _ := os.ReadFile(path)
	var fields map[string]any
	_ = json.Unmarshal(contents, &fields)
	if fields["device_id"] != "old-device" || fields["telemetry_enabled"] != true || fields["smart_routing"] != false || fields["locale"] != "fa-IR" {
		t.Fatalf("preferences lost: %v", fields)
	}
	store.record.Status = "completed"
	fields["user_id"], fields["token"] = 99, "new-login"
	changed, _ := json.Marshal(fields)
	_ = os.WriteFile(path, changed, 0600)
	if err := WriteSettings(dir, store.record); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(changed) {
		t.Fatal("reimported legacy account after later login")
	}
}
