package backend

import (
	"net/http"
	"sync"

	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/common/reporting"
)

var httpClient *http.Client
var mutex = &sync.Mutex{}

// These are the domains we will access via kindling.
var domains = []string{
	"api.iantem.io",
}

func GetHTTPClient() *http.Client {
	mutex.Lock()
	defer mutex.Unlock()
	if httpClient != nil {
		return httpClient
	}

	// Set the client to the kindling client.
	k := kindling.NewKindling(
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithDomainFronting("https://raw.githubusercontent.com/getlantern/lantern-binaries/refs/heads/main/fronted.yaml.gz", ""),
		kindling.WithProxyless(domains...),
	)
	httpClient = k.NewHTTPClient()
	return httpClient
}
