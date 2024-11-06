package shadowsocks

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport/multiplex"
)

func TestWrapStreamDialer(t *testing.T) {
	config, err := config.GetConfig()
	require.NoError(t, err, "Failed to get config")

	dialer, err := NewStreamDialer(&transport.TCPDialer{}, config)
	require.NoError(t, err, "Failed to wrap stream dialer")

	dialer, _ = multiplex.NewStreamDialer(dialer, config)

	header := http.Header{}
	header.Add("X-Lantern-Auth-Token", config.AuthToken)
	addr := fmt.Sprintf("%s:%d", config.Addr, config.Port)
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.DialStream(ctx, addr)
			},
			Proxy:              http.ProxyURL(&url.URL{Scheme: "http", Host: addr}),
			ProxyConnectHeader: header,
		},
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest("GET", "https://geo.getiantem.org/lookup/185.228.19.20", nil)
	require.NoError(t, err, "Failed to create request")

	resp, err := client.Do(req)
	require.NoError(t, err, "Failed to make request")

	for key, values := range resp.Header {
		for _, value := range values {
			fmt.Printf("%s: %s\n", key, value)
		}
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Failed to read response body")

	t.Log(string(body))
}
