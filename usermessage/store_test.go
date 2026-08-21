package usermessage

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	wire "github.com/getlantern/common/usermessage"
)

func TestStorePersistsPendingAndSeenByUser(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	state, err := newStore(dir)
	require.NoError(t, err)
	require.NoError(t, state.offer("1", testMessage("display-1", now.Add(time.Hour)), now))
	require.NoError(t, state.offer("2", testMessage("display-2", now.Add(time.Hour)), now))

	reloaded, err := newStore(dir)
	require.NoError(t, err)
	message, err := reloaded.current("1", now)
	require.NoError(t, err)
	require.Equal(t, "display-1", message.DisplayID)
	require.NoError(t, reloaded.acknowledge("1", "display-1", now))
	require.NoError(t, reloaded.acknowledge("1", "display-1", now))
	require.Equal(t, []string{"display-1"}, reloaded.seen("1"))
	require.Empty(t, reloaded.seen("2"))

	reloadedAgain, err := newStore(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"display-1"}, reloadedAgain.seen("1"))
	message, err = reloadedAgain.current("1", now)
	require.NoError(t, err)
	require.Nil(t, message)
}

func TestStoreBoundsSeenIDsAndExpiresPending(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	state, err := newStore(t.TempDir())
	require.NoError(t, err)
	for i := 0; i < wire.MaxSeenDisplayIDs+3; i++ {
		id := fmt.Sprintf("display-%d", i)
		require.NoError(t, state.offer("1", testMessage(id, now.Add(time.Hour)), now))
		require.NoError(t, state.acknowledge("1", id, now))
	}
	seen := state.seen("1")
	require.Len(t, seen, wire.MaxSeenDisplayIDs)
	require.Equal(t, "display-3", seen[0])

	require.NoError(t, state.offer("1", testMessage("expiring", now.Add(time.Minute)), now))
	message, err := state.current("1", now.Add(time.Minute))
	require.NoError(t, err)
	require.Nil(t, message)
	require.ErrorIs(t, state.acknowledge("1", "expiring", now.Add(time.Minute)), ErrMessageNotPending)
}

func TestStoreDoesNotReplaceUnacknowledgedMessage(t *testing.T) {
	now := time.Now()
	state, err := newStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, state.offer("1", testMessage("first", now.Add(time.Hour)), now))
	require.NoError(t, state.offer("1", testMessage("second", now.Add(time.Hour)), now))
	message, err := state.current("1", now)
	require.NoError(t, err)
	require.Equal(t, "first", message.DisplayID)
	require.True(t, errors.Is(state.acknowledge("1", "second", now), ErrMessageNotPending))
}

func TestStoreSanitizesInvalidPersistedSeenIDs(t *testing.T) {
	state, err := newStore(t.TempDir())
	require.NoError(t, err)
	state.state.Users["1"] = &userState{Seen: []string{"valid-id", "invalid id", "valid-id"}}
	state.state.Order = []string{"1"}
	require.NoError(t, state.saveLocked())

	reloaded, err := newStore(filepath.Dir(state.path))
	require.NoError(t, err)
	require.Equal(t, []string{"valid-id"}, reloaded.seen("1"))
}

func TestStoreKeepsPendingWhenAcknowledgmentWriteFails(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	state, err := newStore(dir)
	require.NoError(t, err)
	require.NoError(t, state.offer("1", testMessage("display-1", now.Add(time.Hour)), now))

	validPath := state.path
	state.path = dir
	require.Error(t, state.acknowledge("1", "display-1", now))
	state.path = validPath
	message, err := state.current("1", now)
	require.NoError(t, err)
	require.Equal(t, "display-1", message.DisplayID)
	require.Empty(t, state.seen("1"))
}
