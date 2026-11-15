package backend

import (
	"testing"

	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/radiance/servers"
)

func TestOfflineProbeOrder(t *testing.T) {
	good := &servers.Server{Tag: "good", IsLantern: true, Options: "stale", SelectionHistory: &lbA.TagHistory{LastSuccessDelayMs: 55}}
	failed := &servers.Server{Tag: "failed", IsLantern: true, SelectionHistory: &lbA.TagHistory{ConsecutiveFailures: 2}}
	fresh := &servers.Server{Tag: "fresh", IsLantern: true}
	neverProbedKnownGood := &servers.Server{Tag: "beforeGood", IsLantern: true, SelectionHistory: &lbA.TagHistory{LastSuccessDelayMs: 12}}
	ordered := offlineProbeOrder([]*servers.Server{good, failed, fresh, neverProbedKnownGood, good})

	want := []string{"fresh", "failed", "good", "beforeGood", "good"}
	if len(ordered) != len(want) {
		t.Fatalf("got %d servers, want %d", len(ordered), len(want))
	}
	for i, srv := range ordered {
		if srv.Tag != want[i] {
			t.Fatalf("position %d: got tag %q, want %q", i, srv.Tag, want[i])
		}
	}
}

func TestOfflineProbeOrderStable(t *testing.T) {
	a := &servers.Server{Tag: "a", IsLantern: true}
	b := &servers.Server{Tag: "b", IsLantern: true}
	c := &servers.Server{Tag: "c", IsLantern: true}
	ordered := offlineProbeOrder([]*servers.Server{b, a, c})
	if ordered[0].Tag != "b" || ordered[1].Tag != "a" || ordered[2].Tag != "c" {
		t.Fatalf("stable order not preserved: %q, %q, %q", ordered[0].Tag, ordered[1].Tag, ordered[2].Tag)
	}
}
