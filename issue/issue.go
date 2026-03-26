package issue

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"path/filepath"
	"runtime"
	"time"

	"github.com/getlantern/osversion"
	"github.com/getlantern/timezone"
	"go.opentelemetry.io/otel"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/traces"

	"google.golang.org/protobuf/proto"
)

const (
	maxCompressedSize = 20 * 1024 * 1024 // 20 MB
	tracerName        = "github.com/getlantern/radiance/issue"
)

// IssueReporter is used to send issue reports to backend.
type IssueReporter struct {
	httpClient *http.Client
}

// NewIssueReporter creates a new IssueReporter that can be used to send issue reports
// to the backend.
func NewIssueReporter(httpClient *http.Client) *IssueReporter {
	return &IssueReporter{httpClient: httpClient}
}

type IssueType int

const (
	CannotCompletePurchase IssueType = iota
	CannotSignIn
	SpinnerLoadsEndlessly
	CannotAccessBlockedSites
	Slow
	CannotLinkDevice
	ApplicationCrashes
	Other IssueType = iota + 2
	UpdateFails
)

// // issue text to type mapping
// var issueTypeMap = map[string]IssueType{
// 	"Cannot complete purchase":    CannotCompletePurchase,
// 	"Cannot sign in":              CannotSignIn,
// 	"Spinner loads endlessly":     SpinnerLoadsEndlessly,
// 	"Cannot access blocked sites": CannotAccessBlockedSites,
// 	"Slow":                        Slow,
// 	"Cannot link device":          CannotLinkDevice,
// 	"Application crashes":         ApplicationCrashes,
// 	"Other":                       Other,
// 	"Update fails":                UpdateFails,
// }

type IssueReport struct {
	// Type is one of the predefined issue type strings
	Type        IssueType
	Description string
	Email       string
	CountryCode string
	// device common name
	Device            string
	DeviceID          string
	UserID            string
	SubscriptionLevel string
	Locale            string
	// device alphanumeric name
	Model string
	// AdditionalAttachments is a list of additional files to be attached. The log file will be
	// automatically included.
	AdditionalAttachments []string
}

// Report sends an issue report to lantern-cloud/issue, which is then forwarded to ticket system via API
func (ir *IssueReporter) Report(ctx context.Context, report IssueReport) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "Report")
	defer span.End()
	// set a random email if it's empty
	if report.Email == "" {
		report.Email = "support+" + randStr(8) + "@getlantern.org"
	}

	// userStatus := settings.GetString(settings.UserLevelKey)
	osVersion, err := osversion.GetHumanReadable()
	if err != nil {
		slog.Error("Unable to get OS version", "error", err)
		osVersion = runtime.GOOS + " " + runtime.GOARCH
	}
	r := &ReportIssueRequest{
		Type:              ReportIssueRequest_ISSUE_TYPE(report.Type),
		AppVersion:        common.Version,
		Platform:          common.Platform,
		CountryCode:       report.CountryCode,
		SubscriptionLevel: report.SubscriptionLevel,
		Description:       report.Description,
		UserEmail:         report.Email,
		DeviceId:          report.DeviceID,
		UserId:            report.UserID,
		Device:            report.Device,
		Model:             report.Model,
		Language:          report.Locale,
		OsVersion:         osVersion,
	}

	logPath := filepath.Join(settings.GetString(settings.LogPathKey), internal.LogFileName)
	archive, err := buildIssueArchive(logPath, report.AdditionalAttachments, maxCompressedSize)
	if err != nil {
		slog.Error("failed to build issue archive", "error", err)
	}
	if len(archive) > 0 {
		r.Attachments = []*ReportIssueRequest_Attachment{{
			Type:    "application/zip",
			Name:    "logs.zip",
			Content: archive,
		}}
	}

	// send message to lantern-cloud
	out, err := proto.Marshal(r)
	if err != nil {
		slog.Error("unable to marshal issue report", "error", err)
		return fmt.Errorf("error marshaling proto: %w", err)
	}

	issueURL := common.GetBaseURL() + "/issue"
	req, err := newIssueRequest(
		ctx,
		http.MethodPost,
		issueURL,
		bytes.NewReader(out),
	)
	if err != nil {
		slog.Error("unable to create issue report request", "error", err)
		return traces.RecordError(ctx, err)
	}

	resp, err := ir.httpClient.Do(req)
	if err != nil {
		slog.Error("failed to send issue report", "error", err, "requestURL", issueURL)
		return traces.RecordError(ctx, err)
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, err := httputil.DumpResponse(resp, true)
		if err != nil {
			slog.Debug("failed to dump response", "error", err, "responseStatus", resp.StatusCode)
		}
		slog.Error("issue report failed", "statusCode", resp.StatusCode, "response", string(b))
		return traces.RecordError(ctx, fmt.Errorf("issue report failed with status code %d", resp.StatusCode))
	}

	slog.Debug("issue report sent")
	return nil
}

// newIssueRequest creates a new HTTP request with the required headers for issue reporting.
func newIssueRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := common.NewRequestWithHeaders(ctx, method, url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("content-type", "application/x-protobuf")
	req.Header.Set(common.SupportedDataCapsHeader, "monthly,weekly,daily")
	if tz, err := timezone.IANANameForTime(time.Now()); err == nil {
		req.Header.Set(common.TimeZoneHeader, tz)
	}

	return req, nil
}

func randStr(n int) string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	var hexStr string
	for range n {
		hexStr += fmt.Sprintf("%x", r.Intn(16))
	}
	return hexStr
}
