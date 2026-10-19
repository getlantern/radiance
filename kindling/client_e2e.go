//go:build lantern_e2e

package kindling

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/getlantern/radiance/common"
)

// Init validates the required local API endpoint without starting remote transports.
func Init() {
	common.GetBaseURL()
}

// HTTPClient returns a direct client restricted to the configured loopback API.
func HTTPClient() *http.Client {
	endpoint, _ := url.Parse(common.GetBaseURL())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Timeout:   common.DefaultHTTPTimeout,
		Transport: localTransport{origin: endpoint, Transport: transport},
	}
}

type localTransport struct {
	*http.Transport
	origin *url.URL
}

func (t localTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.origin.Scheme || req.URL.Host != t.origin.Host || req.URL.User != nil {
		return nil, errors.New("E2E HTTP request must target the configured local API")
	}
	return t.Transport.RoundTrip(req)
}
