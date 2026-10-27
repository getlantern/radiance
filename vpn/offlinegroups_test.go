package vpn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	box "github.com/getlantern/lantern-box"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rlog "github.com/getlantern/radiance/log"
)

func TestOfflineURLTestsReconcileFilteredGroups(t *testing.T) {
	var hits atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(probe.Close)
	outbounds := []O.Outbound{
		{Tag: "unbounded", Type: lbC.TypeUnbounded, Options: &lbO.UnboundedOutboundOptions{
			InsecureDoNotVerifyClientCert: true,
		}},
		{Tag: "working", Type: C.TypeDirect, Options: &O.DirectOutboundOptions{}},
		{Tag: "selector", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{
			Outbounds: []string{"unbounded", "direct", "working"}, Default: "unbounded",
		}},
		{Tag: "parent", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{
			Outbounds: []string{"empty", "builtin-only", "selector"}, Default: "empty",
		}},
		{Tag: "empty", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{Outbounds: []string{"unbounded"}}},
		{Tag: "builtin-only", Type: C.TypeSelector, Options: &O.SelectorOutboundOptions{
			Outbounds: []string{"direct", "block", AutoSelectTag, ManualSelectTag},
		}},
	}
	ctx, cancel := context.WithTimeout(box.Context(t.Context()), 5*time.Second)
	defer cancel()
	original, err := json.MarshalContext(ctx, outbounds)
	require.NoError(t, err)
	urls := map[string]string{"working": probe.URL, "selector": probe.URL, "parent": probe.URL}
	client := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	results, err := client.RunOfflineURLTests(ctx, "", outbounds, urls)
	require.NoError(t, err)
	require.Len(t, results, 3)
	for tag := range urls {
		assert.Contains(t, results, tag)
	}
	assert.Positive(t, hits.Load())
	after, err := json.MarshalContext(ctx, outbounds)
	require.NoError(t, err)
	assert.JSONEq(t, string(original), string(after))
}
