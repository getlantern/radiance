package issue

import (
	"context"
	"testing"

	"github.com/getlantern/fronted"
	"github.com/getlantern/kindling"

	"github.com/getlantern/radiance/common"

	"github.com/stretchr/testify/require"
)

func TestSendReport(t *testing.T) {
	f := fronted.NewFronted(
		fronted.WithConfigURL("https://raw.githubusercontent.com/getlantern/lantern-binaries/refs/heads/main/fronted.yaml.gz"),
	)
	k := kindling.NewKindling(
		kindling.WithDomainFronting(f),
		kindling.WithProxyless("api.iantem.io"),
	)
	userConfig := common.NewUserConfig("radiance-test", "", "")
	reporter, err := NewIssueReporter(k.NewHTTPClient(), userConfig)
	require.NoError(t, err)
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
