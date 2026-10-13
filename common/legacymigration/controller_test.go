package legacymigration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	var saved *Record
	_ = json.Unmarshal(raw, &saved)
	s.record = saved
	s.saves++
	return nil
}

type uncertainStore struct {
	memoryStore
	failFlush bool
}

func (s *uncertainStore) Save(record *Record) error {
	if s.record != nil && !compatibleEnrollmentUpdate(s.record, record) {
		return ErrConflict
	}
	if err := s.memoryStore.Save(record); err != nil {
		return err
	}
	if s.failFlush {
		s.failFlush = false
		return errors.New("flush unavailable")
	}
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
		func(s *State) { s.UserLevel = "unknown" }, func(s *State) { s.Healthy = false },
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

func adoptedFixture(t *testing.T) (*Controller, *memoryStore, []byte, State) {
	t.Helper()
	controller, store, raw := fixture(t)
	if _, err := controller.Adopt(context.Background(), store.record.SourceSID, raw); err != nil {
		t.Fatal(err)
	}
	state := State{UserID: 42, Token: "secret", DeviceID: "old-device", Locale: "fa-IR", UserLevel: "pro", AutoReport: true, ProxyAll: true, AutoLaunch: true, Healthy: true}
	return controller, store, raw, state
}

func TestCompletionScrubsCredentialsAndPreservesReplayAfterRestart(t *testing.T) {
	controller, store, raw, state := adoptedFixture(t)
	state.UserLevel = "free"
	if err := controller.MarkReady(func() State { return state }); err != nil {
		t.Fatalf("subscription refresh prevented completion: %v", err)
	}
	encoded, err := json.Marshal(store.record)
	if err != nil {
		t.Fatal(err)
	}
	if store.record.Request != nil || len(store.record.UserData) != 0 || store.record.TokenSHA256 != fmtDigest("secret") || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "legacyToken") {
		t.Fatalf("completed record retains credentials: %s", encoded)
	}
	restarted, err := NewController(store, func(context.Context, Request) (json.RawMessage, string, error) {
		t.Fatal("completed request verified again")
		return nil, "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	record, err := restarted.Wait(ctx)
	if err != nil || record.Status != "completed" || record.Request != nil {
		t.Fatalf("completed enrollment blocked startup: %+v, %v", record, err)
	}
	if restarted.OwnerSID() != store.record.SourceSID {
		t.Fatal("lost enrolled owner")
	}
	receipt, err := restarted.Adopt(context.Background(), store.record.SourceSID, raw)
	if err != nil || receipt.Status != "adopted" {
		t.Fatalf("completed replay: %+v, %v", receipt, err)
	}
	if _, err := restarted.Adopt(context.Background(), store.record.SourceSID, append(raw, ' ')); !errors.Is(err, ErrConflict) {
		t.Fatalf("completed replay accepted different bytes: %v", err)
	}
	if _, err := restarted.Adopt(context.Background(), "other", raw); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("completed replay accepted another owner: %v", err)
	}
	state.Locale, state.UserLevel = "en-US", "pro"
	state.AutoReport, state.ProxyAll, state.AutoLaunch = false, false, false
	if err := restarted.MarkReady(func() State { return state }); err != nil {
		t.Fatal(err)
	}
	receipt, err = restarted.Status(store.record.SourceSID)
	if err != nil || receipt.Status != "ready" || store.saves != 2 {
		t.Fatalf("saved preferences or tier invalidated completed migration: %+v, %v", receipt, err)
	}
	calls := 0
	restarted.SetRollback(func(context.Context) error { calls++; return nil })
	if _, err := restarted.Rollback(context.Background(), receipt.SourceSID, receipt.MigrationID, receipt.RequestSHA256); err != nil || calls != 1 {
		t.Fatalf("compact receipt failed rollback: %v", err)
	}
	for _, change := range []func(*State){
		func(s *State) { s.UserID++ },
		func(s *State) { s.Token = "new-login" },
		func(s *State) { s.DeviceID = "new-device" },
		func(s *State) { s.Healthy = false },
		func(s *State) { s.UserLevel = "invalid" },
	} {
		changed := state
		change(&changed)
		if err := restarted.MarkReady(func() State { return changed }); err != nil {
			t.Fatalf("later account change prevented startup: %v", err)
		}
		if receipt, err := restarted.Status(store.record.SourceSID); err != nil || receipt.Status == "ready" {
			t.Fatalf("readiness lost live identity binding: %+v, %v", receipt, err)
		}
	}
}

func fmtDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}

func TestCompletionPersistenceFailureRetainsRecoveryCredentials(t *testing.T) {
	controller, store, _, state := adoptedFixture(t)
	store.fail = true
	if err := controller.MarkReady(func() State { return state }); !errors.Is(err, errPersistence) {
		t.Fatalf("missing completion persistence failure: %v", err)
	}
	if controller.Completed() || store.record.Request == nil || controller.record.Request == nil || len(store.record.UserData) == 0 {
		t.Fatal("failed completion discarded pending recovery identity")
	}
	store.fail = false
	if err := controller.MarkReady(func() State { return state }); err != nil || !controller.Completed() {
		t.Fatalf("completion retry failed: %v", err)
	}
}

func TestAdoptionRetriesAnUncertainFlushWithARefreshedProfile(t *testing.T) {
	_, pending, raw := fixture(t)
	store := &uncertainStore{memoryStore: *pending, failFlush: true}
	calls := 0
	controller, err := NewController(store, func(_ context.Context, request Request) (json.RawMessage, string, error) {
		calls++
		level := "pro"
		if calls > 1 {
			level = "free"
		}
		data := json.RawMessage(fmt.Sprintf(`{"legacyID":42,"legacyToken":%q,"profile_revision":%d}`, request.Token, calls))
		return data, level, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sid := store.record.SourceSID
	if _, err := controller.Adopt(context.Background(), sid, raw); !errors.Is(err, errPersistence) {
		t.Fatalf("missing uncertain flush failure: %v", err)
	}
	if store.record.Status != "adopted" || controller.record.Status != "pending" {
		t.Fatal("flush failure did not leave an uncertain adoption")
	}
	replaced := []byte(strings.Replace(string(raw), `"token":"secret"`, `"token":"different"`, 1))
	if _, err := controller.Adopt(context.Background(), sid, replaced); !errors.Is(err, errPersistence) {
		t.Fatalf("uncertain adoption allowed replacement credentials: %v", err)
	}
	if store.record.Request.Token != "secret" || store.saves != 1 {
		t.Fatal("failed retry replaced the already-written identity")
	}
	receipt, err := controller.Adopt(context.Background(), sid, raw)
	if err != nil || receipt.UserLevel != "free" || calls != 3 || store.saves != 2 || controller.record.Status != "adopted" {
		t.Fatalf("identical retry with refreshed profile failed: %+v, %v", receipt, err)
	}
	if !strings.Contains(string(store.record.UserData), `"profile_revision":3`) {
		t.Fatal("identical retry discarded refreshed authenticated profile")
	}
}

func TestMissingLegacyLocaleAllowsNormalLocaleDetection(t *testing.T) {
	controller, store, raw := fixture(t)
	var request Request
	_ = json.Unmarshal(raw, &request)
	request.Locale = ""
	raw, _ = json.Marshal(request)
	if _, err := controller.Adopt(context.Background(), store.record.SourceSID, raw); err != nil {
		t.Fatal(err)
	}
	state := State{UserID: 42, Token: "secret", DeviceID: "old-device", Locale: "en-US", UserLevel: "pro", AutoReport: true, ProxyAll: true, AutoLaunch: true, Healthy: true}
	if err := controller.MarkReady(func() State { return state }); err != nil {
		t.Fatalf("locale detection prevented migration readiness: %v", err)
	}
}

func TestOldCompletedRecordIsScrubbedWithoutRestoringItsAccount(t *testing.T) {
	controller, store, _, _ := adoptedFixture(t)
	store.record.Status = "completed"
	restarted, err := NewController(store, controller.verify)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	changed := []byte(`{"user_id":99,"token":"new-login","locale":"en-US"}`)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(dir, store.record); err != nil {
		t.Fatal(err)
	}
	if err := restarted.MarkReady(func() State { return State{} }); err != nil {
		t.Fatalf("logout prevented old completed-record cleanup: %v", err)
	}
	if store.record.Request != nil || len(store.record.UserData) != 0 {
		t.Fatal("old completed enrollment retained credentials")
	}
	if err := WriteSettings(dir, store.record); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(changed) {
		t.Fatalf("completed enrollment reimported old settings: %q, %v", after, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := WriteSettings(dir, store.record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("compact completed record recreated deleted settings")
	}
}

func TestPersistedReceiptValidation(t *testing.T) {
	controller, store, _, state := adoptedFixture(t)
	if err := controller.MarkReady(func() State { return state }); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Record){
		"receipt version":        func(r *Record) { r.Receipt.Version++ },
		"receipt status":         func(r *Record) { r.Receipt.Status = "ready" },
		"receipt level":          func(r *Record) { r.Receipt.UserLevel = "invalid" },
		"receipt user":           func(r *Record) { r.Receipt.UserID = 0 },
		"receipt device":         func(r *Record) { r.Receipt.DeviceID = "" },
		"receipt SID":            func(r *Record) { r.Receipt.SourceSID = "other" },
		"receipt migration":      func(r *Record) { r.Receipt.MigrationID = strings.Repeat("b", 32) },
		"receipt digest":         func(r *Record) { r.Receipt.RequestSHA256 = strings.Repeat("b", 64) },
		"token fingerprint":      func(r *Record) { r.TokenSHA256 = "invalid" },
		"missing fingerprint":    func(r *Record) { r.TokenSHA256 = "" },
		"residual account":       func(r *Record) { r.UserData = []byte(`{}`) },
		"pending compact record": func(r *Record) { r.Status = "pending" },
		"adopted compact record": func(r *Record) { r.Status = "adopted" },
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(store.record)
			var record Record
			_ = json.Unmarshal(raw, &record)
			mutate(&record)
			if _, err := NewController(&memoryStore{record: &record}, controller.verify); err == nil {
				t.Fatal("loaded an invalid completed enrollment")
			}
		})
	}
}

func TestDirectoryComparisonDoesNotChangeReplayBinding(t *testing.T) {
	controller, store, raw := fixture(t)
	var request Request
	_ = json.Unmarshal(raw, &request)
	request.SourceDirectory = `c:/users/original/AppData/Roaming/./Lantern/`
	changed, _ := json.Marshal(request)
	if _, err := controller.Adopt(context.Background(), store.record.SourceSID, changed); err != nil {
		t.Fatalf("equivalent local path rejected: %v", err)
	}
	if _, err := controller.Adopt(context.Background(), store.record.SourceSID, raw); !errors.Is(err, ErrConflict) {
		t.Fatalf("equivalent path bypassed byte-identical replay: %v", err)
	}
}

func TestEnrollmentUpdatePreservesAcceptedBindingAndCannotRestoreCredentials(t *testing.T) {
	controller, store, _, state := adoptedFixture(t)
	adopted, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	fullCompleted := *adopted
	fullCompleted.Status = "completed"
	if !compatibleEnrollmentUpdate(adopted, &fullCompleted) {
		t.Fatal("legacy completion rejected")
	}
	if err := controller.MarkReady(func() State { return state }); err != nil {
		t.Fatal(err)
	}
	if !compatibleEnrollmentUpdate(adopted, store.record) || !compatibleEnrollmentUpdate(&fullCompleted, store.record) || !compatibleEnrollmentUpdate(store.record, store.record) {
		t.Fatal("credential scrubbing or compact replay rejected")
	}
	if compatibleEnrollmentUpdate(store.record, &fullCompleted) || compatibleEnrollmentUpdate(store.record, adopted) {
		t.Fatal("completed enrollment restored credentials")
	}
	for name, mutate := range map[string]func(*Record){
		"credentials":    func(r *Record) { r.Request.Token = "second" },
		"request digest": func(r *Record) { r.RequestSHA256 = strings.Repeat("b", 64); r.Receipt.RequestSHA256 = r.RequestSHA256 },
		"receipt user":   func(r *Record) { r.Receipt.UserID++; r.Request.UserID++ },
		"receipt device": func(r *Record) { r.Receipt.DeviceID = "second"; r.Request.DeviceID = "second" },
		"source SID":     func(r *Record) { r.SourceSID = "other"; r.Receipt.SourceSID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(adopted)
			var next Record
			_ = json.Unmarshal(raw, &next)
			mutate(&next)
			if compatibleEnrollmentUpdate(adopted, &next) {
				t.Fatal("changed an already-written identity after an uncertain flush")
			}
		})
	}
	next := *store.record
	next.TokenSHA256 = fmtDigest("second")
	if compatibleEnrollmentUpdate(store.record, &next) {
		t.Fatal("compact completion changed credential fingerprint")
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
