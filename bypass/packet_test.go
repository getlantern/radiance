package bypass

import (
	"context"
	"errors"
	"net"
	"strconv"
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

func TestPacketConnReadDeadline(t *testing.T) {
	pc, err := ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close()
	require.NoError(t, pc.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	errc := make(chan error, 1)
	go func() {
		_, _, err := pc.ReadFrom(make([]byte, 64))
		errc <- err
	}()
	select {
	case err := <-errc:
		var ne net.Error
		require.True(t, errors.As(err, &ne) && ne.Timeout(), "got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("ReadFrom ignored its deadline")
	}
}

// A wedged proxy must not stall writes, and Close must still end everything.
func TestPacketConnWedgedProxy(t *testing.T) {
	pc, err := ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ProxyPort)))
	if err != nil {
		pc.Close()
		t.Skipf("bypass port busy: %v", err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // accept, never answer the handshake
		}
	}()
	ProxyStateChanged()

	readErr := make(chan error, 1)
	go func() {
		_, _, err := pc.ReadFrom(make([]byte, 64))
		readErr <- err
	}()
	sink, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer sink.Close()
	start := time.Now()
	for range 5 {
		_, err := pc.WriteTo([]byte("x"), sink.LocalAddr())
		require.NoError(t, err)
	}
	require.Less(t, time.Since(start), time.Second, "writes waited on the wedged proxy")

	start = time.Now()
	require.NoError(t, pc.Close())
	select {
	case err := <-readErr:
		require.ErrorIs(t, err, net.ErrClosed)
	case <-time.After(2 * time.Second):
		t.Fatal("ReadFrom still blocked after Close")
	}
	require.Less(t, time.Since(start), 2*time.Second)
}
