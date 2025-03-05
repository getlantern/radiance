package issue

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httputil"
	"strconv"

	"github.com/getlantern/golog"
	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/backend/apipb"
	"github.com/getlantern/radiance/util"
	"google.golang.org/protobuf/proto"
)

var (
	log        = golog.LoggerFor("issue")
	maxLogSize = 10247680
)

const (
	requestURL = "https://iantem.io/api/v1/issue"
)

type Attachment struct {
	Name string
	Data []byte
}

// Sends an issue report to lantern-cloud/issue, which is then forwarded to ticket system via API
func SendReport(
	deviceID string,
	userID string,
	language string,
	issueType int,
	description string,
	subscriptionLevel string,
	userEmail string,
	appVersion string,
	device string,
	model string,
	osVersion string,
	attachments []*Attachment,
	country string,
) error {
	r := &apipb.ReportIssueRequest{}

	r.Type = apipb.ReportIssueRequest_ISSUE_TYPE(issueType)
	r.CountryCode = country
	r.AppVersion = appVersion
	r.SubscriptionLevel = subscriptionLevel
	r.Platform = app.Platform
	r.Description = description
	r.UserEmail = userEmail
	r.DeviceId = deviceID
	r.UserId = userID
	r.Device = device
	r.Model = model
	r.OsVersion = osVersion
	r.Language = language

	for _, attachment := range attachments {
		r.Attachments = append(r.Attachments, &apipb.ReportIssueRequest_Attachment{
			Type:    "application/zip",
			Name:    attachment.Name,
			Content: attachment.Data,
		})
	}

	// Zip logs
	if maxLogSize > 0 {
		if size, err := util.ParseFileSize(strconv.Itoa(maxLogSize)); err != nil {
			log.Error(err)
		} else {
			log.Debug("zipping log files for issue report")
			buf := &bytes.Buffer{}
			// zip * under folder app.LogDir
			if _, err := zipLogFiles(buf, app.LogDir, size, int64(maxLogSize)); err == nil {
				r.Attachments = append(r.Attachments, &apipb.ReportIssueRequest_Attachment{
					Type:    "application/zip",
					Name:    app.LogDir + ".zip",
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

	req, err := backend.NewIssueRequest(context.Background(), http.MethodPost, requestURL, bytes.NewReader(out))
	if err != nil {
		return log.Errorf("creating request: %w", err)
	}

	resp, err := backend.GetHTTPClient().Do(req)
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
