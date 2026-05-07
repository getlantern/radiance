package issue

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

	reporter := NewIssueReporter(newProtobufTestClient(t, want, assertLogsZipContainsHello))
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

func TestSendReportWithFirstClassAttachment(t *testing.T) {
	settings.InitSettings(t.TempDir())
	defer settings.Reset()

	osVer, err := osversion.GetHumanReadable()
	require.NoError(t, err)

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

	reporter := NewIssueReporter(newMultipartTestClient(t, want, multipartFile{
		fieldName:   attachmentPartName,
		filename:    "screenshot.png",
		contentType: "image/png",
		content:     []byte("png-bytes"),
	}))
	report := IssueReport{
		Type:              CannotAccessBlockedSites,
		Description:       "Description placeholder-test only",
		Email:             "radiancetest@getlantern.org",
		CountryCode:       "US",
		SubscriptionLevel: "free",
		DeviceID:          settings.GetString(settings.DeviceIDKey),
		UserID:            settings.GetString(settings.UserIDKey),
		Locale:            settings.GetString(settings.LocaleKey),
		Device:            "Samsung Galaxy S10",
		Model:             "SM-G973F",
		Attachments: []*Attachment{
			{
				Name: "screenshot.png",
				Type: "image/png",
				Data: []byte("png-bytes"),
			},
		},
	}

	err = reporter.Report(context.Background(), report)
	require.NoError(t, err)
}

func TestSendReportRejectsInvalidFirstClassAttachment(t *testing.T) {
	settings.InitSettings(t.TempDir())
	defer settings.Reset()

	reporter := NewIssueReporter(&http.Client{})
	err := reporter.Report(context.Background(), IssueReport{
		Type:        CannotAccessBlockedSites,
		Description: "validation path",
		Email:       "radiancetest@getlantern.org",
		Attachments: []*Attachment{
			{
				Name: "report.pdf",
				Type: "application/pdf",
				Data: []byte("pdf"),
			},
		},
	})
	require.ErrorContains(t, err, "unsupported screenshot attachment type")
}

// roundTripperFunc allows using a function as http.RoundTripper
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type multipartFile struct {
	fieldName   string
	filename    string
	contentType string
	content     []byte
}

func newMultipartTestClient(t *testing.T, want *ReportIssueRequest, wantFile multipartFile) *http.Client {
	t.Helper()

	return &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			require.True(t, strings.HasPrefix(req.Header.Get("Content-Type"), "multipart/form-data"))

			got, files := decodeMultipartRequest(t, req)
			require.True(t, proto.Equal(want, got), "received report should match expected report")
			require.Len(t, files, 1)
			assert.Equal(t, wantFile, files[0])

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}
}

func decodeMultipartRequest(t *testing.T, r *http.Request) (*ReportIssueRequest, []multipartFile) {
	t.Helper()
	defer r.Body.Close()

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)

	reader := multipart.NewReader(r.Body, params["boundary"])
	var request *ReportIssueRequest
	var files []multipartFile

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		content, err := io.ReadAll(part)
		require.NoError(t, err)

		switch part.FormName() {
		case requestPartName:
			var got ReportIssueRequest
			err = proto.Unmarshal(content, &got)
			require.NoError(t, err)
			request = &got
		case attachmentPartName:
			files = append(files, multipartFile{
				fieldName:   part.FormName(),
				filename:    part.FileName(),
				contentType: part.Header.Get("Content-Type"),
				content:     content,
			})
		default:
			t.Fatalf("unexpected multipart form field: %s", part.FormName())
		}
	}

	require.NotNil(t, request)
	return request, files
}

func newProtobufTestClient(t *testing.T, want *ReportIssueRequest, validate func(*testing.T, *ReportIssueRequest)) *http.Client {
	t.Helper()

	return &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			require.Equal(t, "application/x-protobuf", req.Header.Get("Content-Type"))

			got := decodeProtoRequest(t, req.Body)
			if validate != nil {
				validate(t, got)
			}

			got.Attachments = nil
			require.True(t, proto.Equal(want, got), "received report should match expected report")

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}
}

func decodeProtoRequest(t *testing.T, body io.ReadCloser) *ReportIssueRequest {
	t.Helper()
	defer body.Close()

	payload, err := io.ReadAll(body)
	require.NoError(t, err)

	var got ReportIssueRequest
	err = proto.Unmarshal(payload, &got)
	require.NoError(t, err)
	return &got
}

func assertLogsZipContainsHello(t *testing.T, got *ReportIssueRequest) {
	t.Helper()

	var found bool
	for _, att := range got.Attachments {
		if att.Name != "logs.zip" {
			continue
		}
		zr, err := zip.NewReader(bytes.NewReader(att.Content), int64(len(att.Content)))
		require.NoError(t, err)
		for _, f := range zr.File {
			if f.Name != "attachments/Hello.txt" {
				continue
			}
			rc, err := f.Open()
			require.NoError(t, err)
			data, err := io.ReadAll(rc)
			require.NoError(t, err)
			require.NoError(t, rc.Close())
			assert.Equal(t, "Hello World", string(data))
			found = true
		}
	}
	assert.True(t, found, "logs.zip should contain attachments/Hello.txt")
}
