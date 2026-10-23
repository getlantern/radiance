package tlsidentity

import (
	"encoding/pem"
	"net/http/httptest"
	"testing"

	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckUnbounded(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	t.Cleanup(server.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	invalidCertificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid DER")}))
	for _, test := range []struct {
		name       string
		ca         string
		serverName string
		wantError  string
	}{
		{name: "missing identity", wantError: "requires egress_ca"},
		{name: "missing CA", serverName: "example.com", wantError: "requires egress_ca"},
		{name: "missing name", ca: ca, wantError: "requires egress_server_name"},
		{name: "malformed CA", ca: "not a certificate", serverName: "example.com", wantError: "PEM certificate"},
		{name: "invalid certificate", ca: invalidCertificate, serverName: "example.com", wantError: "PEM certificate"},
		{name: "configured identity", ca: ca, serverName: "example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := lbO.UnboundedOutboundOptions{
				EgressCA: test.ca, EgressServerName: test.serverName,
				InsecureDoNotVerifyClientCert: true, InsecureDoNotVerifyDiscoveryCert: true,
			}
			original := input
			for _, value := range []any{input, &input} {
				err := CheckUnbounded(value)
				if test.wantError != "" {
					require.ErrorContains(t, err, test.wantError)
				} else {
					require.NoError(t, err)
				}
				assert.Equal(t, original, input)
			}
		})
	}
	var typedNil *lbO.UnboundedOutboundOptions
	require.ErrorContains(t, CheckUnbounded(typedNil), "requires egress_ca")
	require.NoError(t, CheckUnbounded(nil))
	require.NoError(t, CheckUnbounded(&option.DirectOutboundOptions{}))
}
