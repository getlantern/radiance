package issue

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/getlantern/osversion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/kindling"
)

func TestSendReport(t *testing.T) {
	settings.InitSettings(t.TempDir())
	defer settings.Reset()
	// Get OS version for expected report
	osVer, err := osversion.GetHumanReadable()
	require.NoError(t, err)

	// Build expected report
	want := &ReportIssueRequest{
		Type:              ReportIssueRequest_NO_ACCESS,
		CountryCode:       "US",
		AppVersion:        common.Version,
		SubscriptionLevel: "free",
		Platform:          common.Platform,
		Description:       "Description placeholder-test only",
		UserEmail:         "radiancetest@getlantern.org",
		DeviceId:          settings.GetString(settings.DeviceIDKey),
		UserId:            strconv.FormatInt(settings.GetInt64(settings.UserIDKey), 10),
		Device:            "Samsung Galaxy S10",
		Model:             "SM-G973F",
		OsVersion:         osVer,
		Language:          settings.GetString(settings.LocaleKey),
		Attachments: []*ReportIssueRequest_Attachment{
			{
				Type:    "application/zip",
				Name:    "Hello.txt",
				Content: []byte("Hello World"),
			},
		},
	}

	srv := newTestServer(t, want)
	defer srv.Close()

	reporter := &IssueReporter{}
	kindling.SetHTTPClient(newTestClient(t, srv.URL))
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

	err = reporter.Report(context.Background(), report, "radiancetest@getlantern.org", "US")
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

// testServer wraps httptest.Server and holds the expected report for comparison
type testServer struct {
	*httptest.Server
	want *ReportIssueRequest
}

func newTestServer(t *testing.T, want *ReportIssueRequest) *testServer {
	ts := &testServer{want: want}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read and unmarshal the request body
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err, "should read request body")

		var got ReportIssueRequest
		err = proto.Unmarshal(body, &got)
		require.NoError(t, err, "should unmarshal protobuf request")

		// Filter got.Attachments to only include the ones we're testing
		// (exclude logs.zip and other dynamic attachments)
		filteredAttachments := make([]*ReportIssueRequest_Attachment, 0)
		for _, gotAtt := range got.Attachments {
			for _, wantAtt := range ts.want.Attachments {
				if gotAtt.Name == wantAtt.Name {
					filteredAttachments = append(filteredAttachments, gotAtt)
					break
				}
			}
		}
		got.Attachments = filteredAttachments

		// Compare received report with expected report using proto.Equal
		if assert.True(t, proto.Equal(ts.want, &got), "received report should match expected report") {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	return ts
}
