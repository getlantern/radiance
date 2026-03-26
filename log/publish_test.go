package log

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushlisher(t *testing.T) {
	p := newPublisher(10)

	ch, unsub := p.subscribe()
	defer unsub()

	entry := LogEntry{Time: "2025-01-01 00:00:00.000 UTC", Level: "INFO", Message: "hello"}
	p.publish(entry)

	select {
	case got := <-ch:
		assert.Equal(t, entry, got)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broadcast")
	}
}

func TestMultipleSubscribers(t *testing.T) {
	p := newPublisher(10)

	ch1, unsub1 := p.subscribe()
	defer unsub1()
	ch2, unsub2 := p.subscribe()
	defer unsub2()

	entry := LogEntry{Time: "2025-01-01 00:00:00.000 UTC", Level: "DEBUG", Message: "multi"}
	p.publish(entry)

	for _, ch := range []chan LogEntry{ch1, ch2} {
		select {
		case got := <-ch:
			assert.Equal(t, entry, got)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for broadcast")
		}
	}
}

func TestUnsubscribe(t *testing.T) {
	p := newPublisher(10)

	ch, unsub := p.subscribe()
	unsub()

	p.publish(LogEntry{Time: "2025-01-01 00:00:00.000 UTC", Level: "INFO", Message: "after unsub"})

	select {
	case <-ch:
		t.Fatal("should not receive after unsubscribe")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestRingBuffer(t *testing.T) {
	p := newPublisher(3)

	// Fill the ring buffer with 5 entries, so only the last 3 should be available.
	for i := range 5 {
		p.publish(LogEntry{
			Time:    "t",
			Level:   "INFO",
			Message: string(rune('a' + i)),
		})
	}

	ch, unsub := p.subscribe()
	defer unsub()

	// New subscriber should get the 3 ring buffer entries.
	var msgs []string
	for range 3 {
		select {
		case e := <-ch:
			msgs = append(msgs, e.Message)
		case <-time.After(time.Second):
			t.Fatal("timed out reading ring buffer entries")
		}
	}
	assert.Equal(t, []string{"c", "d", "e"}, msgs)
}

func TestConcurrentBroadcast(t *testing.T) {
	p := newPublisher(100)
	ch, unsub := p.subscribe()
	defer unsub()

	var wg sync.WaitGroup
	n := 50
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			p.publish(LogEntry{Time: "t", Level: "INFO", Message: "msg"})
		}(i)
	}
	wg.Wait()

	received := 0
	for {
		select {
		case <-ch:
			received++
		default:
			require.Equal(t, n, received)
			return
		}
	}
}
