package issue

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/getlantern/radiance/common"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSendReport(t *testing.T) {
	var receivedReport *ReportIssueRequest
	srv := newTestServer(t, &receivedReport)
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

	// Validate the received report
	require.NotNil(t, receivedReport, "server should have received a report")
	require.Equal(t, ReportIssueRequest_NO_ACCESS, receivedReport.Type, "issue type should match")
	require.Equal(t, "Description placeholder-test only", receivedReport.Description, "description should match")
	require.Equal(t, "radiancetest@getlantern.org", receivedReport.UserEmail, "email should match")
	require.Equal(t, "US", receivedReport.CountryCode, "country code should match")
	require.Equal(t, "Samsung Galaxy S10", receivedReport.Device, "device should match")
	require.Equal(t, "SM-G973F", receivedReport.Model, "model should match")
	require.Equal(t, common.Version, receivedReport.AppVersion, "app version should match")
	require.Equal(t, common.Platform, receivedReport.Platform, "platform should match")
	require.Equal(t, userConfig.DeviceID(), receivedReport.DeviceId, "device ID should match")
	
	// Validate attachments (should have the test attachment plus logs)
	require.GreaterOrEqual(t, len(receivedReport.Attachments), 1, "should have at least the test attachment")
	
	// Find and validate the Hello.txt attachment
	var foundHelloTxt bool
	for _, att := range receivedReport.Attachments {
		if att.Name == "Hello.txt" {
			foundHelloTxt = true
			require.Equal(t, []byte("Hello World"), att.Content, "attachment content should match")
			require.Equal(t, "application/zip", att.Type, "attachment type should be application/zip")
		}
	}
	require.True(t, foundHelloTxt, "Hello.txt attachment should be present")
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

func newTestServer(t *testing.T, receivedReport **ReportIssueRequest) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate request method
		require.Equal(t, http.MethodPost, r.Method, "request method should be POST")
		
		// Validate content type
		require.Equal(t, "application/x-protobuf", r.Header.Get("content-type"), "content type should be application/x-protobuf")
		
		// Read and unmarshal the request body
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err, "should read request body")
		
		var report ReportIssueRequest
		err = proto.Unmarshal(body, &report)
		require.NoError(t, err, "should unmarshal protobuf request")
		
		// Store the received report for validation in the test
		*receivedReport = &report
		
		w.WriteHeader(http.StatusOK)
	}))
}
