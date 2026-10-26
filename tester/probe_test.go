package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchMetadataAndHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/downgrade" {
			http.Redirect(w, r, "http://example.com/", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("origin response"))
	}))
	defer srv.Close()
	cert := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	t.Setenv("CURL_CA_BUNDLE", cert)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("ALL_PROXY", "")
	dir := t.TempDir()
	require.NoError(t, fetch(context.Background(), srv.URL, "", dir, time.Second))
	data, err := os.ReadFile(filepath.Join(dir, "fetch.json"))
	require.NoError(t, err)
	var stats struct {
		Status int `json:"http_code"`
		Exit   int `json:"exitcode"`
	}
	require.NoError(t, json.Unmarshal(data, &stats))
	assert.Equal(t, 503, stats.Status)
	assert.Zero(t, stats.Exit)
	_, err = os.Stat(filepath.Join(dir, "success"))
	require.NoError(t, err)
	dir = t.TempDir()
	require.Error(t, fetch(context.Background(), srv.URL+"/downgrade", "", dir, time.Second))
	_, err = os.Stat(filepath.Join(dir, "success"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Error(t, fetch(context.Background(), "http://example.com", "", t.TempDir(), time.Second))
}

func TestRunRejectsMissingPinnedTarget(t *testing.T) {
	t.Setenv("DATA", t.TempDir())
	t.Setenv("TARGET_URL", "https://example.com")
	t.Setenv("RADIANCE_ENV", "staging")
	t.Setenv("STICKY", "true")
	t.Setenv("TARGET_TAG", "")
	t.Setenv("DIRECT", "false")
	assert.ErrorContains(t, run(t.Context()), "TARGET_TAG")
	t.Setenv("TARGET_TAG", "assigned")
	assert.ErrorContains(t, run(t.Context()), "sticky config")
}
