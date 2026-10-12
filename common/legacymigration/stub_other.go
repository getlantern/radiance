//go:build !windows

package legacymigration

import (
	"errors"
	"net"
)

var errWindowsRequired = errors.New("legacy user adoption requires Windows")

// OpenStore is unavailable outside Windows.
func OpenStore() (Store, error) { return nil, errWindowsRequired }

// Prepare is unavailable outside Windows.
func Prepare(sid, source, id string) error { return errWindowsRequired }

// Listen is unavailable outside Windows.
func Listen() (net.Listener, error) { return nil, errWindowsRequired }

// PeerSID is unavailable outside Windows.
func PeerSID(conn net.Conn) (string, error) { return "", errWindowsRequired }

// ProtectDataDir is unavailable outside Windows.
func ProtectDataDir(path string) error { return errWindowsRequired }
