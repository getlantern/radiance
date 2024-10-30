package radiance

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/config"
)

func Test_runDirect(t *testing.T) {
	config.SetConfig("split:3|logger:")
	transport := config.GetConfig()
	addr := "https://www.google.com/robots.txt"
	run(transport, addr)
}

func Test_Run(t *testing.T) {
	config.SetConfig("split:3|logger:")
	addr := "localhost:8080"

	radiance, err := New()
	require.NoError(t, err)

	shutdown, err := radiance.Run(addr)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := shutdown(ctx)
		require.NoError(t, err)
	}()

	proxyURL, err := url.Parse("http://" + addr)
	require.NoError(t, err)

	client := http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: time.Second * 10,
	}

	rURL := "https://www.google.com/robots.txt"

	resp, err := client.Head(rURL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var headers string
	for k, v := range resp.Header {
		headers += fmt.Sprintf("%s: %s\n", k, v)
	}

	t.Logf("Response:\n%v\n", headers)
}
