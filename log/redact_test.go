package log

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"

	lblog "github.com/getlantern/lantern-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helperTag = "helper-0123456789ab"

// Clients must not export helper- outbound tags, and logs are uploaded with
// issue reports and streamed to the UI.
func TestLogOutputRedactsHelperTags(t *testing.T) {
	var buf bytes.Buffer
	w := newRedactingWriter(&buf)
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{ReplaceAttr: replaceLogAttr}))
	logger.Info("selected "+helperTag, "tag", helperTag,
		"error", errors.New("outbound/unbounded["+helperTag+"]: dial failed"))
	out := buf.String()
	assert.NotContains(t, out, helperTag)
	assert.Contains(t, out, "outbound/unbounded[helper]: dial failed")
}

func TestRedactingWriterReportsTheCallersLength(t *testing.T) {
	var buf bytes.Buffer
	w := newRedactingWriter(&buf)
	in := []byte("x " + helperTag + " y\n")
	n, err := w.Write(in)
	require.NoError(t, err)
	assert.Equal(t, len(in), n, "io.Writer contract: n counts the input")
	assert.Equal(t, "x helper y\n", buf.String())

	buf.Reset()
	plain := []byte("no tags here\n")
	n, err = w.Write(plain)
	require.NoError(t, err)
	assert.Equal(t, len(plain), n)
	assert.Equal(t, "no tags here\n", buf.String())
}

// Ruling R3: a sing-box dial error names the volunteer's address next to the
// helper outbound.
func TestLogOutputRedactsVolunteerAddressesOnHelperLines(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(newRedactingWriter(&buf), &slog.HandlerOptions{ReplaceAttr: replaceLogAttr}))
	logger.Warn("dial failed", "error", errors.New("outbound/samizdat["+helperTag+"]: dial tcp 203.0.113.7:443"))
	logger.Warn("dial failed", "error", errors.New("outbound/samizdat["+helperTag+"]: dial tcp [2001:db8::7]:443"))
	logger.Warn("dial failed", "error", errors.New("outbound/samizdat[lantern-1]: dial tcp 198.51.100.9:443"))
	out := buf.String()
	assert.NotContains(t, out, "203.0.113.7")
	assert.NotContains(t, out, "2001:db8::7")
	assert.Contains(t, out, "198.51.100.9:443", "lines without a helper outbound keep their addresses")
}

// The tunnel names live-added outbounds' loggers "outbound/<tag>[<type>]", and
// lantern-box's factory redacts the tag in that name before the line reaches
// the writer, so the line names its helper outbound only as
// "outbound/helper[<type>]".
func TestLogOutputRedactsAddressesUnderTunnelHelperLoggers(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(newRedactingWriter(&buf), &slog.HandlerOptions{ReplaceAttr: replaceLogAttr})
	f := lblog.NewFactory(h)
	f.NewLogger("outbound/" + helperTag + "[samizdat]").Error("dial tcp 203.0.113.7:443: connection refused")
	f.NewLogger("endpoint/" + helperTag + "[wireguard]").Error("handshake with [2001:db8::7]:51820 failed")
	f.NewLogger("outbound/lantern-1[samizdat]").Error("dial tcp 198.51.100.9:443: connection refused")
	out := buf.String()
	assert.NotContains(t, out, helperTag)
	assert.NotContains(t, out, "203.0.113.7")
	assert.NotContains(t, out, "2001:db8::7")
	assert.Contains(t, out, "198.51.100.9:443", "lines without a helper outbound keep their addresses")
}
