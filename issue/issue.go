package issue

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"strconv"
	"time"

	"github.com/getlantern/golog"
	"github.com/getlantern/jibber_jabber"
	"github.com/getlantern/osversion"

	"github.com/getlantern/radiance/api"
	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"

	"google.golang.org/protobuf/proto"
)

var (
	log        = golog.LoggerFor("issue")
	maxLogSize = 10247680
)

const (
	requestURL = "https://iantem.io/api/v1/issue"
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
}

// NewIssueReporter creates a new IssueReporter that can be used to send issue reports
// to the backend.
func NewIssueReporter(httpClient *http.Client, subHandler SubscriptionHandler, userConfig common.UserInfo) (*IssueReporter, error) {
	if httpClient == nil {
		return nil, fmt.Errorf("httpClient is nil")
	}
	if subHandler == nil {
		return nil, fmt.Errorf("user is nil")
	}
	return &IssueReporter{httpClient: httpClient, subHandler: subHandler, userConfig: userConfig}, nil
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
		log.Errorf("Error while getting user subscription info: %v", err)
	} else if sub.Tier == api.TierPro {
		subLevel = "pro"
	}
	osVersion, err := osversion.GetHumanReadable()
	if err != nil {
		log.Errorf("Unable to get OS version: %v", err)
	}
	userLocale, err := jibber_jabber.DetectIETF()
	if err != nil || userLocale == "C" {
		log.Debugf("Ignoring OS locale and using default")
		userLocale = "en-US"
	}

	r := &ReportIssueRequest{}
	r.Type = ReportIssueRequest_ISSUE_TYPE(issueType)
	r.CountryCode = country
	r.AppVersion = app.Version
	r.SubscriptionLevel = subLevel
	r.Platform = app.Platform
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
	if maxLogSize > 0 {
		if size, err := parseFileSize(strconv.Itoa(maxLogSize)); err != nil {
			log.Error(err)
		} else {
			log.Debug("zipping log files for issue report")
			buf := &bytes.Buffer{}
			// zip * under folder app.LogDir
			if _, err := zipLogFiles(buf, logDir, size, int64(maxLogSize)); err == nil {
				r.Attachments = append(r.Attachments, &ReportIssueRequest_Attachment{
					Type:    "application/zip",
					Name:    "logs.zip",
					Content: buf.Bytes(),
				})
			} else {
				log.Errorf("unable to zip log files: %v", err)
			}
		}
	}

	// send message to lantern-cloud
	out, err := proto.Marshal(r)
	if err != nil {
		log.Errorf("unable to marshal issue report: %v", err)
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
		return log.Errorf("creating request: %w", err)
	}

	resp, err := ir.httpClient.Do(req)
	if err != nil {
		return log.Errorf("unable to send issue report: %v", err)
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, err := httputil.DumpResponse(resp, true)
		if err != nil {
			log.Debugf("Unable to get failed response body for [%s]", requestURL)
		}
		return log.Errorf("Bad response status: %d | response:\n%#v", resp.StatusCode, string(b))
	}

	log.Debugf("issue report sent: %v", resp)
	return nil
}
