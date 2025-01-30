package transport

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/Jigsaw-Code/outline-sdk/transport"
)

// AtomicStreamDialer is a [transport.StreamDialer] that allows dynamically updating
// the underlying dialer at runtime. This enables seamless switching to a new dialer
// when configuration changes or a better connection is available. It is safe for
// concurrent use by multiple goroutines.
type AtomicStreamDialer struct {
	// dialer is an atomic.Value that holds the current StreamDialer.
	dialer atomic.Value
}

// NewAtomicStreamDialer creates a new AtomicStreamDialer with the initial [transport.StreamDialer].
func NewAtomicStreamDialer(dialer transport.StreamDialer) (*AtomicStreamDialer, error) {
	d := &AtomicStreamDialer{}
	if err := d.SetDialer(dialer); err != nil {
		return nil, err
	}
	return d, nil
}

// DialStream implements the [transport.StreamDialer] interface.
func (d *AtomicStreamDialer) DialStream(ctx context.Context, remoteAddr string) (transport.StreamConn, error) {
	v := d.dialer.Load()
	sd, ok := v.(transport.StreamDialer)
	if !ok {
		return nil, errors.New("no dialer set")
	}
	return sd.DialStream(ctx, remoteAddr)
}

// SetDialer updates the underlying [transport.StreamDialer].
func (d *AtomicStreamDialer) SetDialer(dialer transport.StreamDialer) error {
	if dialer == nil {
		return errors.New("dialer must not be nil")
	}
	d.dialer.Store(dialer)
	return nil
}
