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

	"github.com/getlantern/jibber_jabber"
	"github.com/getlantern/osversion"

	"github.com/getlantern/radiance/api"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"

	"google.golang.org/protobuf/proto"
)

const (
	requestURL = "https://iantem.io/api/v1/issue"
	maxLogSize = 10247680 // 10 MB
)

var (
	log = slog.Default()
)

type SubscriptionHandler interface {
	Subscription() (api.Subscription, error)
}

// Attachment is a file attachment
type Attachment struct {
	Name string
	Data []byte
}

// IssueReporter is used to send issue reports to backend
type IssueReporter struct {
	httpClient *http.Client
	subHandler SubscriptionHandler
	userConfig common.UserInfo
	log        *slog.Logger
}

// NewIssueReporter creates a new IssueReporter that can be used to send issue reports
// to the backend.
func NewIssueReporter(
	httpClient *http.Client,
	subHandler SubscriptionHandler,
	userConfig common.UserInfo,
	logger *slog.Logger,
) (*IssueReporter, error) {
	if httpClient == nil {
		return nil, fmt.Errorf("httpClient is nil")
	}
	if subHandler == nil {
		return nil, fmt.Errorf("user is nil")
	}
	return &IssueReporter{
		httpClient: httpClient,
		subHandler: subHandler,
		userConfig: userConfig,
		log:        logger,
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

// Report sends an issue report to lantern-cloud/issue, which is then forwarded to ticket system via API
func (ir *IssueReporter) Report(
	logDir, userEmail string,
	issueType int,
	description string,
	attachments []*Attachment,
	device string,
	model string,
	country string,
) error {
	// set a random email if it's empty
	if userEmail == "" {
		userEmail = "support+" + randStr(8) + "@getlantern.org"
	}

	// get subscription level as string
	subLevel := "free"
	sub, err := ir.subHandler.Subscription()
	if err != nil {
		ir.log.Error("getting user subscription info", "error", err)
	} else if sub.Tier == api.TierPro {
		subLevel = "pro"
	}
	osVersion, err := osversion.GetHumanReadable()
	if err != nil {
		ir.log.Error("Unable to get OS version", "error", err)
	}
	userLocale, err := jibber_jabber.DetectIETF()
	if err != nil || userLocale == "C" {
		ir.log.Debug("Ignoring OS locale and using default", "default", "en-US", "error", err)
		userLocale = "en-US"
	}

	r := &ReportIssueRequest{}
	r.Type = ReportIssueRequest_ISSUE_TYPE(issueType)
	r.CountryCode = country
	r.AppVersion = common.Version
	r.SubscriptionLevel = subLevel
	r.Platform = common.Platform
	r.Description = description
	r.UserEmail = userEmail
	r.DeviceId = ir.userConfig.DeviceID()
	r.UserId = strconv.FormatInt(ir.userConfig.LegacyID(), 10)
	r.Device = device
	r.Model = model
	r.OsVersion = osVersion
	r.Language = userLocale

	for _, attachment := range attachments {
		r.Attachments = append(r.Attachments, &ReportIssueRequest_Attachment{
			Type:    "application/zip",
			Name:    attachment.Name,
			Content: attachment.Data,
		})
	}

	// Zip logs
	ir.log.Debug("zipping log files for issue report")
	buf := &bytes.Buffer{}
	// zip * under folder common.LogDir
	if _, err := zipLogFiles(buf, logDir, maxLogSize, int64(maxLogSize)); err == nil {
		r.Attachments = append(r.Attachments, &ReportIssueRequest_Attachment{
			Type:    "application/zip",
			Name:    "logs.zip",
			Content: buf.Bytes(),
		})
	} else {
		ir.log.Error("unable to zip log files", "error", err, "logDir", logDir, "maxSize", maxLogSize)
	}

	// send message to lantern-cloud
	out, err := proto.Marshal(r)
	if err != nil {
		ir.log.Error("unable to marshal issue report", "error", err)
		return err
	}

	req, err := backend.NewIssueRequest(
		context.Background(),
		http.MethodPost,
		requestURL,
		bytes.NewReader(out),
		ir.userConfig,
	)
	if err != nil {
		ir.log.Error("unable to create issue report request", "error", err)
		return err
	}

	resp, err := ir.httpClient.Do(req)
	if err != nil {
		ir.log.Error("failed to send issue report", "error", err, "requestURL", requestURL)
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, err := httputil.DumpResponse(resp, true)
		if err != nil {
			ir.log.Debug("failed to dump response", "error", err, "responseStatus", resp.StatusCode)
		}
		ir.log.Error("issue report failed", "statusCode", resp.StatusCode, "response", string(b))
		return fmt.Errorf("issue report failed with status code %d", resp.StatusCode)
	}

	ir.log.Debug("issue report sent")
	return nil
}
