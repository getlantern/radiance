//go:build !lantern_e2e

package kindling

import (
	"net/http"

	"github.com/getlantern/radiance/common"
)

// Init starts shared transport initialization in the background.
func Init() {
	go ensureInit()
}

// HTTPClient returns an HTTP client whose first request waits for shared transport initialization.
func HTTPClient() *http.Client {
	return &http.Client{
		Timeout:   common.KindlingHTTPTimeout,
		Transport: readyTransport{},
	}
}
