package issue

import (
	"context"
	"testing"

	"github.com/getlantern/fronted"
	"github.com/getlantern/kindling"

	"github.com/getlantern/radiance/api"
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
	reporter, err := NewIssueReporter(k.NewHTTPClient(), &mockSubscriptionHandler{}, userConfig)
	require.NoError(t, err)
	err = reporter.Report(
		context.Background(),
		t.TempDir(),
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

type mockSubscriptionHandler struct{}

func (m *mockSubscriptionHandler) Subscription() (api.Subscription, error) {
	return api.Subscription{}, nil
}
