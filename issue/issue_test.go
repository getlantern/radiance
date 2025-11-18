package issue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/getlantern/radiance/common"

	"github.com/stretchr/testify/require"
)

func TestSendReport(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	userConfig := common.NewUserConfig("radiance-test", "", "")
	reporter := &IssueReporter{
		httpClient: newTestClient(t, srv.URL),
		userConfig: userConfig,
	}
	report := IssueReport{
		Type:        "Cannot access blocked sites",
		Description: "Description placeholder-test only",
		Attachments: []*Attachment{
			{
				Name: "Hello.txt",
				Data: []byte("Hello World"),
			},
		},
		Device: "Samsung Galaxy S10",
		Model:  "SM-G973F",
	}

	err := reporter.Report(context.Background(), report, "radiancetest@getlantern.org", "US")
	require.NoError(t, err)
}

func newTestClient(t *testing.T, testURL string) *http.Client {
	return &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			parsedURL, err := url.Parse(testURL)
			if err != nil {
				t.Fatalf("failed to parse testURL: %v", err)
			}
			req.URL = parsedURL
			return http.DefaultTransport.RoundTrip(req)
		}),
	}
}

// roundTripperFunc allows using a function as http.RoundTripper
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newTestServer(t *testing.T) *httptest.Server {
	// TODO: verify the received report content
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}
