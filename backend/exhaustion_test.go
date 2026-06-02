package backend

import (
	"testing"
	"time"
)

func TestExhaustionGate_AllowRateLimitsBelowGap(t *testing.T) {
	prev := defaultExhaustionRefetchGap
	defaultExhaustionRefetchGap = 50 * time.Millisecond
	t.Cleanup(func() { defaultExhaustionRefetchGap = prev })

	var g exhaustionGate
	if !g.allow() {
		t.Fatal("first allow must pass on a zero gate")
	}
	if g.allow() {
		t.Error("second allow inside the gap must be rate-limited")
	}
	if g.allow() {
		t.Error("third allow inside the gap must still be rate-limited")
	}

	time.Sleep(defaultExhaustionRefetchGap + 10*time.Millisecond)
	if !g.allow() {
		t.Error("allow after the gap elapses must pass again")
	}
	if g.allow() {
		t.Error("post-recovery allow must re-arm the gate")
	}
}
