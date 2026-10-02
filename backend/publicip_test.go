package backend

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
)

func TestDetectPublicIPSkipsWhileTunnelUp(t *testing.T) {
	common.SetPublicIP("")
	done := make(chan struct{})
	go func() {
		detectPublicIP(context.Background(), func() bool { return false })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("detectPublicIP looked up the IP while the tunnel was up")
	}

	req, err := common.NewRequestWithHeaders(context.Background(), http.MethodGet, "https://example.com", nil)
	require.NoError(t, err)
	assert.Empty(t, req.Header.Get(common.ClientIPHeader))
}
