package issue

import (
	"os"
	"testing"

	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/user"
	"github.com/stretchr/testify/require"
)

func TestSendReport(t *testing.T) {
	k := kindling.NewKindling(
		kindling.WithDomainFronting("https://raw.githubusercontent.com/getlantern/lantern-binaries/refs/heads/main/fronted.yaml.gz", ""),
		kindling.WithProxyless("api.iantem.io"),
	)
	user := user.New(k.NewHTTPClient())
	reporter, err := NewIssueReporter(k.NewHTTPClient(), user)
	require.NoError(t, err)
	// Grab a temporary directory
	dir, err := os.MkdirTemp("", "lantern")
	require.NoError(t, err)
	err = reporter.Report(
		dir,
		"radiancetest@getlantern.org",
		int(ReportIssueRequest_NO_ACCESS),
		"Description placeholder-test only",
		[]*Attachment{
			{
				Name: "Hello.txt",
				Data: []byte("Hello World"),
			},
		},
		"Samsung Galaxy S10",
		"SM-G973F",
		"US",
	)
	require.NoError(t, err)
}
