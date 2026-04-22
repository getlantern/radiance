package issue

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/getlantern/osversion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/kindling"
)

func TestSendReport_ProtobufPath(t *testing.T) {
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
		UserId:            strconv.FormatInt(settings.GetInt64(settings.UserIDKey), 10),
		Device:            "Samsung Galaxy S10",
		Model:             "SM-G973F",
		OsVersion:         osVer,
		Language:          settings.GetString(settings.LocaleKey),
		Attachments: []*ReportIssueRequest_Attachment{
			{
				Type:    "text/plain",
				Name:    "Hello.txt",
				Content: []byte("Hello World"),
			},
		},
	}

	reporter := &IssueReporter{}
	kindling.SetKindling(&mockKindling{newValidatingClient(t, testExpectations{
		wantContentTypePrefix: "application/x-protobuf",
		wantProto:             want,
	})})
	report := IssueReport{
		Type:        "Cannot access blocked sites",
		Description: "Description placeholder-test only",
		Attachments: []*Attachment{
			{
				Name: "Hello.txt",
				Type: "text/plain",
				Data: []byte("Hello World"),
			},
		},
		Device: "Samsung Galaxy S10",
		Model:  "SM-G973F",
	}

	err = reporter.Report(context.Background(), report, "radiancetest@getlantern.org", "US")
	require.NoError(t, err)
}

func TestSendReport_MultipartPath(t *testing.T) {
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
		UserId:            strconv.FormatInt(settings.GetInt64(settings.UserIDKey), 10),
		Device:            "Samsung Galaxy S10",
		Model:             "SM-G973F",
		OsVersion:         osVer,
		Language:          settings.GetString(settings.LocaleKey),
		Attachments: []*ReportIssueRequest_Attachment{
			{
				Type:    "text/plain",
				Name:    "context.txt",
				Content: []byte("Support context"),
			},
		},
	}

	reporter := &IssueReporter{}
	kindling.SetKindling(&mockKindling{newValidatingClient(t, testExpectations{
		wantContentTypePrefix: "multipart/form-data",
		wantProto:             want,
		wantMultipartFiles: []multipartFileExpectation{
			{
				FieldName:   attachmentPartName,
				Filename:    "screenshot.png",
				ContentType: "image/png",
				Content:     []byte("png-bytes"),
			},
		},
	})})
	report := IssueReport{
		Type:        "Cannot access blocked sites",
		Description: "Description placeholder-test only",
		Attachments: []*Attachment{
			{
				Name: "context.txt",
				Type: "text/plain",
				Data: []byte("Support context"),
			},
			{
				Name:       "screenshot.png",
				Type:       "image/png",
				Data:       []byte("png-bytes"),
				FirstClass: true,
			},
		},
		Device: "Samsung Galaxy S10",
		Model:  "SM-G973F",
	}

	err = reporter.Report(context.Background(), report, "radiancetest@getlantern.org", "US")
	require.NoError(t, err)
}

func TestSendReport_MultipartPathNormalizesScreenshotFilename(t *testing.T) {
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
		UserId:            strconv.FormatInt(settings.GetInt64(settings.UserIDKey), 10),
		Device:            "Samsung Galaxy S10",
		Model:             "SM-G973F",
		OsVersion:         osVer,
		Language:          settings.GetString(settings.LocaleKey),
	}

	reporter := &IssueReporter{}
	kindling.SetKindling(&mockKindling{newValidatingClient(t, testExpectations{
		wantContentTypePrefix: "multipart/form-data",
		wantProto:             want,
		wantMultipartFiles: []multipartFileExpectation{
			{
				FieldName:   attachmentPartName,
				Filename:    "screenshot.png",
				ContentType: "image/png",
				Content:     []byte("png-bytes"),
			},
		},
	})})
	report := IssueReport{
		Type:        "Cannot access blocked sites",
		Description: "Description placeholder-test only",
		Attachments: []*Attachment{
			{
				Name:       "  screenshot.png  ",
				Type:       "image/png",
				Data:       []byte("png-bytes"),
				FirstClass: true,
			},
		},
		Device: "Samsung Galaxy S10",
		Model:  "SM-G973F",
	}

	err = reporter.Report(context.Background(), report, "radiancetest@getlantern.org", "US")
	require.NoError(t, err)
}

func TestSendReportMultipartValidation(t *testing.T) {
	settings.InitSettings(t.TempDir())
	defer settings.Reset()

	reporter := &IssueReporter{}

	tests := []struct {
		name        string
		attachments []*Attachment
		wantErr     string
	}{
		{
			name: "rejects too many screenshots",
			attachments: []*Attachment{
				{Name: "1.png", Type: "image/png", Data: []byte("1"), FirstClass: true},
				{Name: "2.png", Type: "image/png", Data: []byte("2"), FirstClass: true},
				{Name: "3.png", Type: "image/png", Data: []byte("3"), FirstClass: true},
				{Name: "4.png", Type: "image/png", Data: []byte("4"), FirstClass: true},
			},
			wantErr: "too many screenshot attachments",
		},
		{
			name: "rejects unsupported screenshot types",
			attachments: []*Attachment{
				{Name: "report.pdf", Type: "application/pdf", Data: []byte("pdf"), FirstClass: true},
			},
			wantErr: "unsupported screenshot attachment type",
		},
		{
			name: "rejects oversized total attachment payload",
			attachments: []*Attachment{
				{
					Name:       "oversized.png",
					Type:       "image/png",
					Data:       bytesOfLen(maxIssueAttachmentBytes + 1),
					FirstClass: true,
				},
			},
			wantErr: "total issue attachment size exceeds",
		},
		{
			name: "rejects screenshot names with control characters",
			attachments: []*Attachment{
				{Name: "bad\r\nname.png", Type: "image/png", Data: []byte("1"), FirstClass: true},
			},
			wantErr: "contains invalid control characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := reporter.Report(context.Background(), IssueReport{
				Type:        "Cannot access blocked sites",
				Description: "validation path",
				Attachments: tt.attachments,
				Device:      "Test device",
				Model:       "Model 1",
			}, "radiancetest@getlantern.org", "US")
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func bytesOfLen(n int) []byte {
	return make([]byte, n)
}

func newValidatingClient(t *testing.T, want testExpectations) *http.Client {
	return &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			require.True(
				t,
				strings.HasPrefix(req.Header.Get("Content-Type"), want.wantContentTypePrefix),
				"unexpected content type: %s",
				req.Header.Get("Content-Type"),
			)

			switch {
			case strings.HasPrefix(req.Header.Get("Content-Type"), "application/x-protobuf"):
				got := decodeProtoRequest(t, req.Body)
				assertProtoMatches(t, want.wantProto, got)
			case strings.HasPrefix(req.Header.Get("Content-Type"), "multipart/form-data"):
				gotProto, gotFiles := decodeMultipartRequest(t, req)
				assertProtoMatches(t, want.wantProto, gotProto)
				require.Len(t, gotFiles, len(want.wantMultipartFiles))
				for i, wantFile := range want.wantMultipartFiles {
					gotFile := gotFiles[i]
					assert.Equal(t, wantFile.FieldName, gotFile.FieldName)
					assert.Equal(t, wantFile.Filename, gotFile.Filename)
					assert.Equal(t, wantFile.ContentType, gotFile.ContentType)
					assert.Equal(t, wantFile.Content, gotFile.Content)
				}
			default:
				t.Fatalf("unexpected content type: %s", req.Header.Get("Content-Type"))
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type testExpectations struct {
	wantContentTypePrefix string
	wantProto             *ReportIssueRequest
	wantMultipartFiles    []multipartFileExpectation
}

type multipartFileExpectation struct {
	FieldName   string
	Filename    string
	ContentType string
	Content     []byte
}

func decodeProtoRequest(t *testing.T, body io.ReadCloser) *ReportIssueRequest {
	t.Helper()
	defer body.Close()

	payload, err := io.ReadAll(body)
	require.NoError(t, err, "should read request body")

	var got ReportIssueRequest
	err = proto.Unmarshal(payload, &got)
	require.NoError(t, err, "should unmarshal protobuf request")
	return &got
}

func decodeMultipartRequest(
	t *testing.T,
	r *http.Request,
) (*ReportIssueRequest, []multipartFileExpectation) {
	t.Helper()
	defer r.Body.Close()

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	require.NoError(t, err, "should parse multipart content type")
	require.Equal(t, "multipart/form-data", mediaType)

	reader := multipart.NewReader(r.Body, params["boundary"])
	var gotProto *ReportIssueRequest
	gotFiles := make([]multipartFileExpectation, 0)

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		require.NoError(t, err, "should read multipart body")

		content, err := io.ReadAll(part)
		require.NoError(t, err, "should read multipart part")

		switch part.FormName() {
		case requestPartName:
			var request ReportIssueRequest
			err = proto.Unmarshal(content, &request)
			require.NoError(t, err, "should unmarshal request part")
			gotProto = &request
		case attachmentPartName:
			gotFiles = append(gotFiles, multipartFileExpectation{
				FieldName:   part.FormName(),
				Filename:    part.FileName(),
				ContentType: part.Header.Get("Content-Type"),
				Content:     content,
			})
		default:
			t.Fatalf("unexpected multipart form field: %s", part.FormName())
		}
	}

	require.NotNil(t, gotProto, "multipart payload should include request part")
	return gotProto, gotFiles
}

func assertProtoMatches(
	t *testing.T,
	want *ReportIssueRequest,
	got *ReportIssueRequest,
) {
	t.Helper()
	got.Attachments = filterExpectedAttachments(got.Attachments, want.Attachments)
	if !assert.True(
		t,
		proto.Equal(want, got),
		"received report should match expected report",
	) {
		t.Fatalf("protobuf payload mismatch")
	}
}

func filterExpectedAttachments(
	got []*ReportIssueRequest_Attachment,
	want []*ReportIssueRequest_Attachment,
) []*ReportIssueRequest_Attachment {
	filtered := make([]*ReportIssueRequest_Attachment, 0, len(want))
	for _, gotAttachment := range got {
		for _, wantAttachment := range want {
			if gotAttachment.Name == wantAttachment.Name {
				filtered = append(filtered, gotAttachment)
				break
			}
		}
	}
	return filtered
}

type mockKindling struct {
	c *http.Client
}

func (m *mockKindling) NewHTTPClient() *http.Client {
	return m.c
}

func (m *mockKindling) ReplaceTransport(name string, rt func(ctx context.Context, addr string) (http.RoundTripper, error)) error {
	panic("not implemented")
}
