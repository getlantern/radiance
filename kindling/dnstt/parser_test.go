package dnstt

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getlantern/radiance/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func gzipYAML(yaml []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(yaml)
	gz.Close()
	return buf.Bytes()
}

func TestDNSTTConfigUpdate(t *testing.T) {
	validYAML := []byte(`
dnsttConfigs:
  - dohResolver: https://localhost/dns
    domain: "example.com"
`)
	invalidGzip := []byte("not a gzip file")

	tests := []struct {
		name         string
		yaml         []byte
		status       int
		expectUpdate bool
	}{
		{
			name:         "empty configURL",
			yaml:         nil,
			status:       200,
			expectUpdate: false,
		},
		{
			name:         "valid config",
			yaml:         gzipYAML(validYAML),
			status:       200,
			expectUpdate: true,
		},
		{
			name:         "invalid gzip",
			yaml:         invalidGzip,
			status:       200,
			expectUpdate: false,
		},
		{
			name:         "http error",
			yaml:         nil,
			status:       404,
			expectUpdate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updated := make(chan struct{})
			defer close(updated)
			if tt.expectUpdate {
				events.Subscribe(func(e DNSTTUpdateEvent) {
					assert.NotEmpty(t, e.YML)
					updated <- struct{}{}
				})
			}

			// Custom RoundTripper to mock HTTP responses
			rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				resp := &http.Response{
					StatusCode: tt.status,
					Header:     make(http.Header),
					Request:    req,
				}
				if tt.status == 200 && tt.yaml != nil {
					resp.Body = io.NopCloser(bytes.NewReader(tt.yaml))
				} else {
					resp.Body = http.NoBody
				}
				return resp, nil
			})

			client := &http.Client{Transport: rt}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dnsttConfigUpdate(ctx, filepath.Join(t.TempDir(), "dnstt.yml.gz"), client)
			if tt.expectUpdate {
				assert.Eventually(t, func() bool {
					_, ok := <-updated
					return ok
				}, 1*time.Second, 10*time.Millisecond, "onNewDNSTTConfig should be called")
			}
		})
	}
}

const validDNSTTYAML = `
dnsttConfigs:
  - domain: t.example.com
    publicKey: 405eb9e22d806e3a0a8e667c6665a321c8a6a35fa680ed814716a66d7ad84977
    dohResolver: https://dns.example/dns-query
`

const invalidDNSTTYAML = `
dnsttConfigs:
  - domain: t.example.com
    publicKey:
`

func TestDNSTTOptions(t *testing.T) {
	logger := bytes.NewBuffer(nil)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelDebug,
	})))
	t.Run("embedded config only", func(t *testing.T) {
		opts, closers, err := DNSTTOptions(context.Background(), "", logger)
		assert.NoError(t, err)

		assert.Len(t, opts, maxDNSTTOptions)
		assert.Len(t, closers, maxDNSTTOptions)

		for _, closeFn := range closers {
			assert.NoError(t, closeFn())
		}
	})

	t.Run("local config overrides embedded config", func(t *testing.T) {
		tmp, err := os.CreateTemp(t.TempDir(), "dnstt-*.yml.gz")
		require.NoError(t, err)
		defer tmp.Close()
		_, err = tmp.Write(gzipYAML([]byte(validDNSTTYAML)))
		require.NoError(t, err)
		opts, closers, err := DNSTTOptions(context.Background(), tmp.Name(), logger)
		assert.NoError(t, err)
		assert.Len(t, opts, 1)
		assert.Len(t, closers, 1)

		for _, closeFn := range closers {
			assert.NoError(t, closeFn())
		}
	})

	t.Run("invalid local config falls back to embedded", func(t *testing.T) {
		dir := t.TempDir()
		tmp, err := os.CreateTemp(dir, "dnstt-invalid-*.yml.gz")
		require.NoError(t, err)
		defer tmp.Close()

		_, err = tmp.Write(gzipYAML([]byte(invalidDNSTTYAML)))
		require.NoError(t, err)
		opts, closers, err := DNSTTOptions(context.Background(), tmp.Name(), logger)
		assert.NoError(t, err)
		assert.Len(t, opts, maxDNSTTOptions)
		assert.Len(t, closers, maxDNSTTOptions)
		for _, closeFn := range closers {
			assert.NoError(t, closeFn())
		}
	})

	t.Run("context cancellation does not block", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, closers, err := DNSTTOptions(ctx, "", logger)
		assert.NoError(t, err)
		for _, closeFn := range closers {
			assert.NoError(t, closeFn())
		}
	})
}
