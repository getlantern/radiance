package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/helper"
	"github.com/getlantern/radiance/vpn"
)

var thisMonth = time.Now().In(helper.LocalZone()).Format("2006-01")

var savedUsage = `{"since":"2026-01-02T03:04:05Z","month":"` + thisMonth + `",` +
	`"lifetime":{"proxyless":{"up":1,"down":2},"personal":{"up":3,"down":4},"private":{"up":5,"down":6},"lantern":{"up":7,"down":8},"direct":{"up":9,"down":10}},` +
	`"this_month":{"proxyless":{"up":0,"down":0},"personal":{"up":0,"down":0},"private":{"up":0,"down":0},"lantern":{"up":11,"down":12},"direct":{"up":0,"down":0}}}`

func usageBackend(t *testing.T) *backend.LocalBackend {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	require.NoError(t, os.WriteFile(path, []byte(savedUsage), 0o600))
	b := &backend.LocalBackend{}
	b.UseUsageStatsForTest(vpn.NewUsageStats(path))
	return b
}

func TestVPNUsageEndpoints(t *testing.T) {
	b := usageBackend(t)
	api := newLocalAPI(b, false)

	w := serveHelper(t, api, http.MethodGet, vpnUsageEndpoint, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, savedUsage, w.Body.String())

	w = serveHelper(t, api, http.MethodGet, vpnUsageResetEndpoint, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "reset takes a POST")
	w = serveHelper(t, api, http.MethodPost, vpnUsageResetEndpoint, nil)
	require.Equal(t, http.StatusOK, w.Code)

	w = serveHelper(t, api, http.MethodGet, vpnUsageEndpoint, nil)
	var u vpn.Usage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &u))
	assert.Zero(t, u.Lifetime)
	assert.Zero(t, u.ThisMonth)
	assert.Equal(t, thisMonth, u.Month)
	assert.True(t, u.Since.After(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), "a reset counts from now")
}

func TestVPNUsageClientRoundTrip(t *testing.T) {
	b := usageBackend(t)
	c := newClient()
	c.http.Transport = localTransport{api: newLocalAPI(b, false)}

	u, err := c.VPNUsage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, thisMonth, u.Month)
	assert.Equal(t, vpn.UsageBytes{Up: 5, Down: 6}, u.Lifetime.Private)
	assert.Equal(t, vpn.UsageBytes{Up: 11, Down: 12}, u.ThisMonth.Lantern)

	require.NoError(t, c.ResetVPNUsage(context.Background()))
	u, err = c.VPNUsage(context.Background())
	require.NoError(t, err)
	assert.Zero(t, u.Lifetime)
}

func TestVPNUsageWithoutStore(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, false)
	w := serveHelper(t, api, http.MethodGet, vpnUsageEndpoint, nil)
	require.Equal(t, http.StatusOK, w.Code)
	w = serveHelper(t, api, http.MethodPost, vpnUsageResetEndpoint, nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUsageErrorStatus(t *testing.T) {
	assert.Equal(t, http.StatusServiceUnavailable, usageErrorStatus(fmt.Errorf("read: %w", vpn.ErrUsageBusy)), "a save in progress is retryable")
	assert.Equal(t, http.StatusInternalServerError, usageErrorStatus(errors.New("disk")))
}
