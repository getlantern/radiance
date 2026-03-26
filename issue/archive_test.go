package issue

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotLogFile(t *testing.T) {
	t.Run("reads full file when small", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "test.log")
		content := "line1\nline2\nline3\n"
		require.NoError(t, os.WriteFile(logPath, []byte(content), 0644))

		data, err := snapshotLogFile(logPath, 1024*1024)
		require.NoError(t, err)
		assert.Equal(t, content, string(data))
	})

	t.Run("reads only tail when file exceeds cap", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "test.log")

		// maxCompressed=100 → maxRead = 100*20 = 2000
		// Write 5000 bytes so the file exceeds the cap.
		full := strings.Repeat("X", 5000)
		require.NoError(t, os.WriteFile(logPath, []byte(full), 0644))

		data, err := snapshotLogFile(logPath, 100)
		require.NoError(t, err)
		assert.Equal(t, 2000, len(data))
		// Should be the tail of the file.
		assert.Equal(t, full[3000:], string(data))
	})

	t.Run("returns nil for empty file", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "empty.log")
		require.NoError(t, os.WriteFile(logPath, nil, 0644))

		data, err := snapshotLogFile(logPath, 1024*1024)
		require.NoError(t, err)
		assert.Nil(t, data)
	})

	t.Run("returns error for missing file", func(t *testing.T) {
		_, err := snapshotLogFile("/nonexistent/path.log", 1024*1024)
		assert.Error(t, err)
	})

	t.Run("snapshot is stable after file rotation", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "test.log")
		original := "original log content\n"
		require.NoError(t, os.WriteFile(logPath, []byte(original), 0644))

		// Open and snapshot size (simulating what snapshotLogFile does internally).
		f, err := os.Open(logPath)
		require.NoError(t, err)
		defer f.Close()

		fi, err := f.Stat()
		require.NoError(t, err)
		size := fi.Size()

		// Simulate rotation: rename the file and create a new one.
		require.NoError(t, os.Rename(logPath, logPath+".1"))
		require.NoError(t, os.WriteFile(logPath, []byte("new log content\n"), 0644))

		// The original fd should still read the original data.
		data := make([]byte, size)
		n, err := f.Read(data)
		require.NoError(t, err)
		assert.Equal(t, original, string(data[:n]))
	})
}

func TestReadExtraFiles(t *testing.T) {
	t.Run("reads existing files", func(t *testing.T) {
		dir := t.TempDir()
		f1 := filepath.Join(dir, "a.txt")
		f2 := filepath.Join(dir, "b.txt")
		require.NoError(t, os.WriteFile(f1, []byte("aaa"), 0644))
		require.NoError(t, os.WriteFile(f2, []byte("bbb"), 0644))

		files := readExtraFiles([]string{f1, f2})
		require.Len(t, files, 2)
		assert.Equal(t, "a.txt", files[0].name)
		assert.Equal(t, "aaa", string(files[0].data))
		assert.Equal(t, "b.txt", files[1].name)
		assert.Equal(t, "bbb", string(files[1].data))
	})

	t.Run("skips missing files", func(t *testing.T) {
		dir := t.TempDir()
		existing := filepath.Join(dir, "exists.txt")
		require.NoError(t, os.WriteFile(existing, []byte("data"), 0644))

		files := readExtraFiles([]string{"/no/such/file", existing})
		require.Len(t, files, 1)
		assert.Equal(t, "exists.txt", files[0].name)
	})

	t.Run("nil input returns nil", func(t *testing.T) {
		files := readExtraFiles(nil)
		assert.Nil(t, files)
	})
}

func TestWriteArchive(t *testing.T) {
	t.Run("log only", func(t *testing.T) {
		logData := []byte("some log content")
		buf, err := writeArchive(logData, nil)
		require.NoError(t, err)

		entries := readZipEntries(t, buf.Bytes())
		require.Len(t, entries, 1)
		assert.Equal(t, logArchiveName, entries[0].name)
		assert.Equal(t, "some log content", entries[0].content)
	})

	t.Run("log with extras", func(t *testing.T) {
		logData := []byte("log line")
		extras := []extraFile{
			{name: "config.json", data: []byte(`{"key":"val"}`)},
			{name: "screenshot.png", data: []byte("fake png")},
		}
		buf, err := writeArchive(logData, extras)
		require.NoError(t, err)

		entries := readZipEntries(t, buf.Bytes())
		require.Len(t, entries, 3)
		assert.Equal(t, logArchiveName, entries[0].name)
		assert.Equal(t, "attachments/config.json", entries[1].name)
		assert.Equal(t, "attachments/screenshot.png", entries[2].name)
	})

	t.Run("extras only", func(t *testing.T) {
		extras := []extraFile{{name: "file.txt", data: []byte("hello")}}
		buf, err := writeArchive(nil, extras)
		require.NoError(t, err)

		entries := readZipEntries(t, buf.Bytes())
		require.Len(t, entries, 1)
		assert.Equal(t, "attachments/file.txt", entries[0].name)
	})

	t.Run("empty inputs", func(t *testing.T) {
		buf, err := writeArchive(nil, nil)
		require.NoError(t, err)
		// Should produce a valid but empty zip.
		entries := readZipEntries(t, buf.Bytes())
		assert.Empty(t, entries)
	})
}

func TestFitArchive(t *testing.T) {
	t.Run("everything fits", func(t *testing.T) {
		logData := []byte("small log")
		extras := []extraFile{{name: "a.txt", data: []byte("small")}}
		result, err := fitArchive(logData, extras, 1024*1024)
		require.NoError(t, err)
		require.NotNil(t, result)

		entries := readZipEntries(t, result)
		assert.Len(t, entries, 2)
	})

	t.Run("nil log and nil extras returns nil", func(t *testing.T) {
		result, err := fitArchive(nil, nil, 1024*1024)
		require.NoError(t, err)
		assert.Nil(t, result)
	})

	t.Run("extras dropped when too large", func(t *testing.T) {
		logData := []byte("log data")
		// Make an extra that's big enough to push past a small maxSize.
		bigExtra := extraFile{name: "big.bin", data: bytes.Repeat([]byte{0xFF}, 50*1024)}

		// Find the compressed size of just the log.
		logOnly, err := writeArchive(logData, nil)
		require.NoError(t, err)
		maxSize := int64(logOnly.Len()) + 100 // just barely enough for log, not the extra

		result, err := fitArchive(logData, []extraFile{bigExtra}, maxSize)
		require.NoError(t, err)

		entries := readZipEntries(t, result)
		require.Len(t, entries, 1)
		assert.Equal(t, logArchiveName, entries[0].name)
		assert.Equal(t, "log data", entries[0].content)
	})

	t.Run("log truncated to tail when too large", func(t *testing.T) {
		// Use incompressible random data (2MB) with a budget that fits ~1-2
		// chunks (256KB each) but not the full log.
		logData := make([]byte, 2*1024*1024) // 2MB
		_, err := rand.Read(logData)
		require.NoError(t, err)

		maxSize := int64(512 * 1024) // 512KB

		result, err := fitArchive(logData, nil, maxSize)
		require.NoError(t, err)
		assert.LessOrEqual(t, int64(len(result)), maxSize)

		entries := readZipEntries(t, result)
		require.Len(t, entries, 1)
		assert.Equal(t, logArchiveName, entries[0].name)

		// The included content should be a tail of the original.
		content := entries[0].content
		assert.True(t, len(content) < len(logData), "log should be truncated")
		assert.Equal(t, string(logData[len(logData)-len(content):]), content,
			"included content should be the tail of the original log")
	})

	t.Run("extras only when no log", func(t *testing.T) {
		extras := []extraFile{
			{name: "a.txt", data: []byte("aaa")},
			{name: "b.txt", data: []byte("bbb")},
		}
		result, err := fitArchive(nil, extras, 1024*1024)
		require.NoError(t, err)

		entries := readZipEntries(t, result)
		assert.Len(t, entries, 2)
	})
}

func TestSearchMaxLogTail(t *testing.T) {
	t.Run("all fits", func(t *testing.T) {
		logData := []byte("small log data")
		tailSize := searchMaxLogTail(logData, 1024*1024)
		assert.Equal(t, len(logData), tailSize)
	})

	t.Run("truncates incompressible data", func(t *testing.T) {
		logData := make([]byte, 1024*1024) // 1MB random
		_, err := rand.Read(logData)
		require.NoError(t, err)

		maxSize := int64(300 * 1024) // 300KB
		tailSize := searchMaxLogTail(logData, maxSize)
		assert.Greater(t, tailSize, 0)
		assert.Less(t, tailSize, len(logData))

		// Verify the result actually fits.
		buf, err := writeArchive(logData[len(logData)-tailSize:], nil)
		require.NoError(t, err)
		assert.LessOrEqual(t, int64(buf.Len()), maxSize)
	})
}

func TestAddExtrasGreedily(t *testing.T) {
	t.Run("adds all when they fit", func(t *testing.T) {
		logData := []byte("log")
		extras := []extraFile{
			{name: "a.txt", data: []byte("aaa")},
			{name: "b.txt", data: []byte("bbb")},
		}
		result, err := addExtrasGreedily(logData, extras, 1024*1024)
		require.NoError(t, err)

		entries := readZipEntries(t, result)
		assert.Len(t, entries, 3)
	})

	t.Run("skips extras that would exceed limit", func(t *testing.T) {
		logData := []byte("log")
		small := extraFile{name: "small.txt", data: []byte("s")}
		big := extraFile{name: "big.bin", data: bytes.Repeat([]byte{0xFF}, 50*1024)}

		// Budget enough for log + small, but not big.
		bufWithSmall, err := writeArchive(logData, []extraFile{small})
		require.NoError(t, err)
		maxSize := int64(bufWithSmall.Len()) + 50 // tight budget

		result, err := addExtrasGreedily(logData, []extraFile{small, big}, maxSize)
		require.NoError(t, err)

		entries := readZipEntries(t, result)
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.name
		}
		assert.Contains(t, names, logArchiveName)
		assert.Contains(t, names, "attachments/small.txt")
		assert.NotContains(t, names, "attachments/big.bin")
	})

	t.Run("no extras returns log only", func(t *testing.T) {
		logData := []byte("log content")
		result, err := addExtrasGreedily(logData, nil, 1024*1024)
		require.NoError(t, err)

		entries := readZipEntries(t, result)
		require.Len(t, entries, 1)
		assert.Equal(t, logArchiveName, entries[0].name)
	})
}

func TestBuildIssueArchive(t *testing.T) {
	t.Run("end to end with log and extras", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "lantern.log")
		require.NoError(t, os.WriteFile(logPath, []byte("log line 1\nlog line 2\n"), 0644))

		extra := filepath.Join(dir, "extra.txt")
		require.NoError(t, os.WriteFile(extra, []byte("extra content"), 0644))

		result, err := buildIssueArchive(logPath, []string{extra}, 1024*1024)
		require.NoError(t, err)
		require.NotNil(t, result)

		entries := readZipEntries(t, result)
		require.Len(t, entries, 2)
		assert.Equal(t, logArchiveName, entries[0].name)
		assert.Equal(t, "log line 1\nlog line 2\n", entries[0].content)
		assert.Equal(t, "attachments/extra.txt", entries[1].name)
	})

	t.Run("missing log file still includes extras", func(t *testing.T) {
		dir := t.TempDir()
		extra := filepath.Join(dir, "extra.txt")
		require.NoError(t, os.WriteFile(extra, []byte("data"), 0644))

		result, err := buildIssueArchive(filepath.Join(dir, "nonexistent.log"), []string{extra}, 1024*1024)
		require.NoError(t, err)
		require.NotNil(t, result)

		entries := readZipEntries(t, result)
		require.Len(t, entries, 1)
		assert.Equal(t, "attachments/extra.txt", entries[0].name)
	})

	t.Run("archive respects maxSize", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "lantern.log")
		// Write incompressible data (2MB).
		logContent := make([]byte, 2*1024*1024)
		_, err := rand.Read(logContent)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(logPath, logContent, 0644))

		maxSize := int64(512 * 1024)
		result, err := buildIssueArchive(logPath, nil, maxSize)
		require.NoError(t, err)
		assert.LessOrEqual(t, int64(len(result)), maxSize)

		// Verify it contains the tail.
		entries := readZipEntries(t, result)
		require.Len(t, entries, 1)
		content := entries[0].content
		assert.Equal(t, string(logContent[len(logContent)-len(content):]), content)
	})

	t.Run("snapshot excludes data written after call", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "lantern.log")
		original := "before snapshot\n"
		require.NoError(t, os.WriteFile(logPath, []byte(original), 0644))

		// Snapshot the file.
		data, err := snapshotLogFile(logPath, 1024*1024)
		require.NoError(t, err)

		// Append after snapshot.
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
		require.NoError(t, err)
		_, err = f.WriteString("after snapshot\n")
		require.NoError(t, err)
		f.Close()

		// Snapshot should only contain original content.
		assert.Equal(t, original, string(data))
	})
}

// --- test helpers ---

type zipEntry struct {
	name    string
	content string
}

func readZipEntries(t *testing.T, data []byte) []zipEntry {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	var entries []zipEntry
	for _, f := range r.File {
		rc, err := f.Open()
		require.NoError(t, err)
		body, err := io.ReadAll(rc)
		require.NoError(t, err)
		rc.Close()
		entries = append(entries, zipEntry{name: f.Name, content: string(body)})
	}
	return entries
}
