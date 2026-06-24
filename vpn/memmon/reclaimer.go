package memmon

import (
	"time"

	"github.com/gofrs/uuid/v5"
)

// Reclaimer is the set of side effects the executor performs. It is an interface so the executor can
// be driven by a fake in tests — verifying eviction order, batch sizing, and the hard-close path
// without a live tunnel — while production code can provide tracker-backed reclamation.
type Reclaimer interface {
	// ConnectionsOldestFirst returns the live routed connections sorted by creation time, oldest first.
	ConnectionsOldestFirst() []ConnectionRef
	// CloseConn is a no-op when id is no longer live.
	CloseConn(id uuid.UUID)
	// CloseAllConnections closes every tracked connection for a hard reclaim.
	CloseAllConnections()
	// FreeOSMemory returns freed pages to the OS (runtime/debug.FreeOSMemory).
	FreeOSMemory()
	// OpenConnectionCount is the current count of live routed connections.
	OpenConnectionCount() int
	// TotalDialedConnections is the count of all tracked dialed connections, for the crash dump.
	TotalDialedConnections() int
}

// ConnectionRef is the minimal connection identity the executor needs to close oldest-first.
type ConnectionRef struct {
	ID        uuid.UUID
	CreatedAt time.Time
}
