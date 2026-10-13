package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/getlantern/radiance/common/legacymigration"
	"github.com/stretchr/testify/require"
)

type daemonEnrollmentStore struct {
	record *legacymigration.Record
	saves  int
}

func (s *daemonEnrollmentStore) Load() (*legacymigration.Record, error) { return s.record, nil }
func (s *daemonEnrollmentStore) Save(record *legacymigration.Record) error {
	s.record = record
	s.saves++
	return nil
}

func TestPendingAdoptionAllowsLaterRetry(t *testing.T) {
	store := &daemonEnrollmentStore{record: &legacymigration.Record{
		Version: 1, MigrationID: strings.Repeat("a", 32), SourceSID: "S-1-5-21-123",
		SourceDirectory: `C:\Users\Original\Lantern`, Status: "pending",
	}}
	controller, err := legacymigration.NewController(store, func(context.Context, legacymigration.Request) (json.RawMessage, string, error) {
		return json.RawMessage(`{"legacyID":42}`), "free", nil
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		record, err := waitForLegacyAdoption(ctx, controller, 0)
		if err == nil && (record == nil || record.Status != "adopted") {
			t.Error("missing durable adoption")
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("pending adoption stopped before retry: %v", err)
	default:
	}
	require.Zero(t, store.saves)
	raw, err := json.Marshal(legacymigration.Request{
		Version: 1, MigrationID: store.record.MigrationID, SourceDirectory: store.record.SourceDirectory,
		UserID: 42, Token: "original-token", DeviceID: "original-device",
	})
	require.NoError(t, err)
	_, err = controller.Adopt(ctx, store.record.SourceSID, raw)
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("authenticated retry did not resume startup")
	}
	require.Equal(t, 1, store.saves)
}

func TestPendingAdoptionStopsOnShutdown(t *testing.T) {
	store := &daemonEnrollmentStore{record: &legacymigration.Record{
		Version: 1, MigrationID: strings.Repeat("a", 32), SourceSID: "S-1-5-21-123",
		SourceDirectory: `C:\Users\Original\Lantern`, Status: "pending",
	}}
	controller, err := legacymigration.NewController(store, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = waitForLegacyAdoption(ctx, controller, time.Minute)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, store.saves)
}

func TestDaemonPreservesTelemetryConsent(t *testing.T) {
	require.True(t, daemonBackendOptions("data", "logs", "info").PreserveTelemetryConsent)
}
