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
	"github.com/getlantern/radiance/transport/logger"
	"github.com/getlantern/radiance/transport/multiplex"
)

func TestWrapStreamDialer(t *testing.T) {
	config, err := config.GetConfig()
	require.NoError(t, err, "Failed to get config")

	socksConfig := config.ShadowsocksCfg
	socksConfig["addr"] = fmt.Sprintf("%s:%d", config.Addr, config.Port)

	logDlr, _ := logger.NewStreamDialer(&transport.TCPDialer{})
	dialer, err := NewStreamDialer(logDlr, socksConfig)
	require.NoError(t, err, "Failed to wrap stream dialer")

	dialer = multiplex.NewStreamDialer(dialer)

	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialStream(ctx, addr)
	}
	onProxyConnectResponse :=
		func(ctx context.Context, proxyURL *url.URL, connectReq *http.Request, connectRes *http.Response) error {
			t.Logf("Request\n%+v", connectReq)
			t.Logf("Response:\n%+v", connectRes)
			return nil
		}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:            dialContext,
			Proxy:                  http.ProxyURL(&url.URL{Scheme: "http", Host: socksConfig["addr"]}),
			OnProxyConnectResponse: onProxyConnectResponse,
		},
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest("HEAD", "https://lantern.io/", nil)
	ruri, err := client.Transport.(*http.Transport).Proxy(req)
	require.NoError(t, err, "Failed to get proxy")
	t.Logf("url: %s, host: %s, ruri: %s", req.URL, req.Host, ruri)
	require.NoError(t, err, "Failed to create request")

	resp, err := client.Transport.RoundTrip(req)
	require.NoError(t, err, "Failed to make request")

	t.Log("Response:")
	for k, v := range resp.Header {
		t.Logf("%s: %v", k, v)
	}

	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Failed to copy response body")

	t.Logf("\n\n%s", b)
}

func addHeaders(req *http.Request) {
	req.Header.Add("X-Lantern-Auth-Token", "")
}
