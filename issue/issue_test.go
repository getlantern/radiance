package issue

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/getlantern/osversion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
)

func TestSendReport(t *testing.T) {
	settings.InitSettings(t.TempDir())
	defer settings.Reset()
	// Get OS version for expected report
	osVer, err := osversion.GetHumanReadable()
	require.NoError(t, err)

	// Create a temp file to use as an additional attachment
	tmpDir := t.TempDir()
	attachPath := filepath.Join(tmpDir, "Hello.txt")
	err = os.WriteFile(attachPath, []byte("Hello World"), 0644)
	require.NoError(t, err)

	// Build expected report (without attachments — we verify those separately)
	want := &ReportIssueRequest{
		Type:              ReportIssueRequest_NO_ACCESS,
		CountryCode:       "US",
		AppVersion:        common.Version,
		SubscriptionLevel: "free",
		Platform:          common.Platform,
		Description:       "Description placeholder-test only",
		UserEmail:         "radiancetest@getlantern.org",
		DeviceId:          settings.GetString(settings.DeviceIDKey),
		UserId:            settings.GetString(settings.UserIDKey),
		Device:            "Samsung Galaxy S10",
		Model:             "SM-G973F",
		OsVersion:         osVer,
		Language:          settings.GetString(settings.LocaleKey),
	}

	srv := newTestServer(t, want)
	defer srv.Close()

	reporter := NewIssueReporter(&http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			parsedURL, err := url.Parse(srv.URL)
			require.NoError(t, err, "failed to parse test server URL")
			req.URL = parsedURL
			return http.DefaultTransport.RoundTrip(req)
		}),
	})
	report := IssueReport{
		Type:                  CannotAccessBlockedSites,
		Description:           "Description placeholder-test only",
		Email:                 "radiancetest@getlantern.org",
		CountryCode:           "US",
		SubscriptionLevel:     "free",
		DeviceID:              settings.GetString(settings.DeviceIDKey),
		UserID:                settings.GetString(settings.UserIDKey),
		Locale:                settings.GetString(settings.LocaleKey),
		Device:                "Samsung Galaxy S10",
		Model:                 "SM-G973F",
		AdditionalAttachments: []string{attachPath},
	}

	err = reporter.Report(context.Background(), report)
	require.NoError(t, err)
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

		// Verify logs.zip attachment contains the additional file
		var foundHello bool
		for _, att := range got.Attachments {
			if att.Name == "logs.zip" {
				zr, err := zip.NewReader(bytes.NewReader(att.Content), int64(len(att.Content)))
				require.NoError(t, err, "should open logs.zip")
				for _, f := range zr.File {
					if f.Name == "attachments/Hello.txt" {
						rc, err := f.Open()
						require.NoError(t, err)
						data, err := io.ReadAll(rc)
						require.NoError(t, err)
						rc.Close()
						assert.Equal(t, "Hello World", string(data))
						foundHello = true
					}
				}
			}
		}
		assert.True(t, foundHello, "logs.zip should contain attachments/Hello.txt")

		// Clear attachments for field-level comparison
		got.Attachments = nil

		// Compare received report with expected report using proto.Equal
		if assert.True(t, proto.Equal(ts.want, &got), "received report should match expected report") {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	return ts
}
