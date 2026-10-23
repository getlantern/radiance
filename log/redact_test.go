package log

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"

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
