package bypass

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPacketConnCloseUnblocksRead(t *testing.T) {
	pc, err := ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	errc := make(chan error, 1)
	go func() {
		_, _, err := pc.ReadFrom(make([]byte, 64))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, pc.Close())
	select {
	case err := <-errc:
		require.True(t, errors.Is(err, net.ErrClosed), "got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("ReadFrom still blocked after Close")
	}
	_, err = pc.WriteTo([]byte("x"), pc.LocalAddr())
	require.ErrorIs(t, err, net.ErrClosed)
}
