package issue

import (
	"testing"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/backend/apipb"
)

func TestSendReport(t *testing.T) {
	err := SendReport(
		"34qsdf-24qsadf-32542q",
		"1",
		"en",
		int(apipb.ReportIssueRequest_NO_ACCESS),
		"Description placeholder-test only",
		"pro",
		"radiancetest@getlantern.org",
		app.Version,
		"Samsung Galaxy S10",
		"SM-G973F",
		"11",
		[]*Attachment{
			{
				Name: "Hello.txt",
				Data: []byte("Hello World"),
			},
		},
		"US",
	)
	if err != nil {
		t.Errorf("SendReport() error = %v", err)
	}
}
