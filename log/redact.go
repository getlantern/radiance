package log

import (
	"bytes"
	"io"

	lbA "github.com/getlantern/lantern-box/adapter"
)

var helperTagPrefix = []byte(lbA.HelperTagPrefix)

// redactingWriter replaces helper- outbound tags with "helper" on their way
// to every log sink: the log file (which issue reports upload), stdout and
// the publisher that streams logs to the UI. A helper tag is a client-side
// pseudonym for one of the user's pairings, and clients must not export it.
type redactingWriter struct {
	w io.Writer
}

func newRedactingWriter(w io.Writer) io.Writer {
	return &redactingWriter{w: w}
}

// Write redacts p and reports len(p) on success, as io.Writer requires, even
// though fewer bytes may reach the sink.
func (r *redactingWriter) Write(p []byte) (int, error) {
	if !bytes.Contains(p, helperTagPrefix) {
		return r.w.Write(p)
	}
	if _, err := io.WriteString(r.w, lbA.RedactHelperTags(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
