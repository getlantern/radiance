package config

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/getlantern/radiance/events"
	"github.com/stretchr/testify/assert"
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
dnstt:
  dohResolver: https://localhost/dns
  domain: "example.com"
`)
	invalidGzip := []byte("not a gzip file")

	tests := []struct {
		name         string
		configURL    string
		yaml         []byte
		status       int
		expectUpdate bool
	}{
		{
			name:         "empty configURL",
			configURL:    "",
			yaml:         nil,
			status:       200,
			expectUpdate: false,
		},
		{
			name:         "valid config",
			configURL:    "/config",
			yaml:         gzipYAML(validYAML),
			status:       200,
			expectUpdate: true,
		},
		{
			name:         "invalid gzip",
			configURL:    "/config",
			yaml:         invalidGzip,
			status:       200,
			expectUpdate: false,
		},
		{
			name:         "http error",
			configURL:    "/notfound",
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
				events.Subscribe(func(e NewDNSTTConfigEvent) {
					assert.NotNil(t, e.New)
					updated <- struct{}{}
				})
			}

			// Custom RoundTripper to mock HTTP responses
			rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if tt.configURL == "" || req.URL.Path != tt.configURL {
					return &http.Response{
						StatusCode: tt.status,
						Body:       http.NoBody,
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}
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

			url := ""
			if tt.configURL != "" {
				url = "http://mock" + tt.configURL
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			DNSTTConfigUpdate(ctx, url, client, 1*time.Minute)
			if tt.expectUpdate {
				assert.Eventually(t, func() bool {
					_, ok := <-updated
					return ok
				}, 1*time.Second, 10*time.Millisecond, "onNewDNSTTConfig should be called")
			}
		})
	}
}
