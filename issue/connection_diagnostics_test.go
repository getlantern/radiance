package issue

import (
	"archive/zip"
	"bytes"
	"io"
	"testing"

	"github.com/getlantern/lantern-box/connectiondiag"
	"github.com/stretchr/testify/require"
)

func TestConnectionDiagnosticsArchive(t *testing.T) {
	connectiondiag.Enable(false)
	data, err := connectiondiag.Snapshot()
	require.NoError(t, err)
	archive, err := buildIssueArchive(t.TempDir(), nil, 1024*1024, extraFile{name: "connection-diagnostics.json", data: data})
	require.NoError(t, err)
	z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	for _, f := range z.File {
		if f.Name == "attachments/connection-diagnostics.json" {
			r, err := f.Open()
			require.NoError(t, err)
			defer r.Close()
			b, err := io.ReadAll(r)
			require.NoError(t, err)
			require.JSONEq(t, string(data), string(b))
			return
		}
	}
	t.Fatal("connection diagnostics missing from report archive")
}

func TestConnectionDiagnosticsTinyBudget(t *testing.T) {
	for _, budget := range []int64{0, 1, 21, 22, 100} {
		data, err := buildIssueArchive(t.TempDir(), nil, budget, extraFile{name: "connection-diagnostics.json", data: []byte(`{"version":1}`)})
		require.NoError(t, err)
		require.LessOrEqual(t, int64(len(data)), budget)
	}
}
