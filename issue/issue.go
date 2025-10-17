package issue

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"strconv"
	"time"

	"github.com/getlantern/common"
	"github.com/getlantern/osversion"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/backend"
	rcommon "github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/traces"
)

const (
	requestURL = "https://iantem.io/api/v1/issue"
	maxLogSize = 20 * 1024 * 1024 // 20 MB
	tracerName = "github.com/getlantern/radiance/issue"
)

// IssueReporter is used to send issue reports to backend
type IssueReporter struct {
	httpClient *http.Client

	userInfo rcommon.UserInfo
}

// NewIssueReporter creates a new IssueReporter that can be used to send issue reports
// to the backend.
func NewIssueReporter(
	httpClient *http.Client,
	userConfig rcommon.UserInfo,
) (*IssueReporter, error) {
	if httpClient == nil {
		return nil, fmt.Errorf("httpClient is nil")
	}
	httpClient.Transport = traces.NewRoundTripper(httpClient.Transport)
	return &IssueReporter{
		httpClient: httpClient,
		userInfo:   userConfig,
	}, nil
}

func randStr(n int) string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	var hexStr string
	for i := 0; i < n; i++ {
		hexStr += fmt.Sprintf("%x", r.Intn(16))
	}
	return hexStr
}

// Attachment is a file attachment
type Attachment struct {
	Name string
	Data []byte
}

type IssueReport struct {
	// Type is one of the predefined issue type strings
	Type string
	// Issue description
	Description string
	// Attachment is a list of issue attachments
	Attachments []*Attachment
	// device common name
	Device string
	// device alphanumeric name
	Model string
}

// issue text to type mapping
var issueTypeMap = map[string]int{
	"Cannot complete purchase":    0,
	"Cannot sign in":              1,
	"Spinner loads endlessly":     2,
	"Cannot access blocked sites": 3,
	"Slow":                        4,
	"Cannot link device":          5,
	"Application crashes":         6,
	"Other":                       9,
	"Update fails":                10,
}

// Report sends an issue report to lantern-cloud/issue, which is then forwarded to ticket system via API
func (ir *IssueReporter) Report(ctx context.Context, report IssueReport, userEmail, country string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "Report")
	defer span.End()
	// set a random email if it's empty
	if userEmail == "" {
		userEmail = "support+" + randStr(8) + "@getlantern.org"
	}

	userStatus := "free"
	userData, err := ir.userInfo.GetData()
	if err != nil {
		slog.Error("Unable to get user data", "error", err)
	} else {
		if userData != nil && userData.Level == common.UserLevelPro {
			userStatus = "pro"
		}
	}

	osVersion, err := osversion.GetHumanReadable()
	if err != nil {
		slog.Error("Unable to get OS version", "error", err)
	}
	// get issue type as integer
	iType, ok := issueTypeMap[report.Type]
	if !ok {
		slog.Error("Unknown issue type, setting to 'Other'", "type", report.Type)
		iType = 9
	}

	r := &ReportIssueRequest{
		Type:              ReportIssueRequest_ISSUE_TYPE(iType),
		CountryCode:       country,
		AppVersion:        rcommon.Version,
		SubscriptionLevel: userStatus,
		Platform:          rcommon.Platform,
		Description:       report.Description,
		UserEmail:         userEmail,
		DeviceId:          ir.userInfo.DeviceID(),
		UserId:            strconv.FormatInt(ir.userInfo.ID(), 10),
		Device:            report.Device,
		Model:             report.Model,
		OsVersion:         osVersion,
		Language:          ir.userInfo.Locale(),
	}

	for _, attachment := range report.Attachments {
		r.Attachments = append(r.Attachments, &ReportIssueRequest_Attachment{
			Type:    "application/zip",
			Name:    attachment.Name,
			Content: attachment.Data,
		})
	}

	// Zip logs
	slog.Debug("zipping log files for issue report")
	buf := &bytes.Buffer{}
	// zip * under folder common.LogDir
	logDir := rcommon.LogPath()
	slog.Debug("zipping log files", "logDir", logDir, "maxSize", maxLogSize)
	if _, zipErr := zipLogFiles(buf, logDir, maxLogSize, int64(maxLogSize)); zipErr == nil {
		r.Attachments = append(r.Attachments, &ReportIssueRequest_Attachment{
			Type:    "application/zip",
			Name:    "logs.zip",
			Content: buf.Bytes(),
		})
	} else {
		slog.Error("unable to zip log files", "error", err, "logDir", logDir, "maxSize", maxLogSize)
		return traces.RecordError(ctx, fmt.Errorf("Could not zip log files: %w", zipErr))
	}

	// send message to lantern-cloud
	out, err := proto.Marshal(r)
	if err != nil {
		slog.Error("unable to marshal issue report", "error", err)
		return traces.RecordError(ctx, fmt.Errorf("Could not marshal issue report %w", err))
	}

	req, err := backend.NewIssueRequest(
		ctx,
		http.MethodPost,
		requestURL,
		bytes.NewReader(out),
		ir.userInfo,
	)
	if err != nil {
		slog.Error("unable to create issue report request", "error", err)
		return traces.RecordError(ctx, err)
	}

	resp, err := ir.httpClient.Do(req)
	if err != nil {
		slog.Error("failed to send issue report", "error", err, "requestURL", requestURL)
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
