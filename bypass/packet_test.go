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

// A proxy started by another process never calls ProxyStateChanged here; a conn
// sending on a plain socket must still find it, and once associated must hand
// back whole payloads into exactly-sized buffers.
func TestPacketConnFindsUnannouncedProxy(t *testing.T) {
	if l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ProxyPort))); err != nil {
		t.Skipf("bypass port busy: %v", err)
	} else {
		l.Close()
	}
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer echo.Close()
	seen := make(chan net.Addr, 64)
	go func() {
		b := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFrom(b)
			if err != nil {
				return
			}
			seen <- from
			echo.WriteTo(b[:n], from)
		}
	}()

	pc, err := ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close()
	newSingboxServer(t) // no ProxyStateChanged: as if another process started it

	payload := make([]byte, 100)
	for i := range payload {
		payload[i] = byte(i)
	}
	local := pc.LocalAddr().(*net.UDPAddr).Port
	require.Eventually(t, func() bool {
		if _, err := pc.WriteTo(payload, echo.LocalAddr()); err != nil {
			return false
		}
		select {
		case from := <-seen:
			return from.(*net.UDPAddr).Port != local // arrived via the proxy
		case <-time.After(200 * time.Millisecond):
			return false
		}
	}, 3*proxyProbeInterval, 50*time.Millisecond, "never moved onto the unannounced proxy")

	n, err := pc.WriteTo(payload, echo.LocalAddr())
	require.NoError(t, err)
	require.Equal(t, len(payload), n, "write count includes the SOCKS header")

	buf := make([]byte, len(payload))
	require.NoError(t, pc.SetReadDeadline(time.Now().Add(5*time.Second)))
	for {
		n, _, err := pc.ReadFrom(buf)
		require.NoError(t, err)
		if n == len(payload) && string(buf) == string(payload) {
			return
		}
		require.Equal(t, len(payload), n, "payload truncated by the SOCKS header")
	}
}

// Canceling an association attempt must interrupt a handshake the proxy never answers.
func TestAssociateCanceledMidHandshake(t *testing.T) {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ProxyPort)))
	if err != nil {
		t.Skipf("bypass port busy: %v", err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err = associate(ctx)
	require.Error(t, err)
	require.Less(t, time.Since(start), time.Second, "handshake ignored cancellation")
}

// Concurrent reads on an association must not race (run with -race).
func TestAssociatedConnConcurrentReads(t *testing.T) {
	if l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ProxyPort))); err != nil {
		t.Skipf("bypass port busy: %v", err)
	} else {
		l.Close()
	}
	newSingboxServer(t)
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer echo.Close()
	go func() {
		b := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFrom(b)
			if err != nil {
				return
			}
			echo.WriteTo(b[:n], from)
		}
	}()
	pc, err := associate(context.Background())
	require.NoError(t, err)
	defer pc.Close()
	const readers, each = 4, 25
	done := make(chan error, readers)
	for range readers {
		go func() {
			b := make([]byte, 64)
			for range each {
				n, from, err := pc.ReadFrom(b)
				if err != nil {
					done <- err
					return
				}
				if from.String() != echo.LocalAddr().String() || string(b[:n]) != "ping" {
					done <- errors.New("bad datagram " + from.String() + " " + string(b[:n]))
					return
				}
			}
			done <- nil
		}()
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				pc.WriteTo([]byte("ping"), echo.LocalAddr())
				time.Sleep(time.Millisecond)
			}
		}
	}()
	for range readers {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("readers starved")
		}
	}
}
