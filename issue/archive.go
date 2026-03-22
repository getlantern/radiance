package issue

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// buildIssueArchive creates a zip archive containing the log file and additional
// attachment files. The total compressed archive size will not exceed maxSize bytes.
//
// Additional files are included only if space permits after the log.
func buildIssueArchive(logPath string, additionalFiles []string, maxSize int64) ([]byte, error) {
	logData, err := snapshotLogFile(logPath, maxSize)
	if err != nil {
		slog.Warn("unable to snapshot log file, trying additional files only", "path", logPath, "error", err)
	}

	extras := readExtraFiles(additionalFiles)

	return fitArchive(logData, extras, maxSize)
}

// snapshotLogFile opens the log file, records its current size, and reads the tail
// up to a reasonable cap.
func snapshotLogFile(logPath string, maxCompressed int64) ([]byte, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	size := fi.Size()
	if size == 0 {
		return nil, nil
	}

	// Cap the amount we read: even with poor compression, we'd never need more
	// than maxCompressed * 20 bytes of uncompressed log to fill the archive.
	maxRead := maxCompressed * 20
	readSize := size
	if readSize > maxRead {
		readSize = maxRead
	}

	// Seek to read only the tail (most recent logs).
	if size > readSize {
		if _, err := f.Seek(size-readSize, io.SeekStart); err != nil {
			return nil, err
		}
	}

	data := make([]byte, readSize)
	n, err := io.ReadFull(f, data)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("reading log file: %w", err)
	}
	return data[:n], nil
}

type extraFile struct {
	name string
	data []byte
}

func readExtraFiles(paths []string) []extraFile {
	var files []extraFile
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			slog.Warn("unable to read additional file", "path", p, "error", err)
			continue
		}
		files = append(files, extraFile{
			name: filepath.Base(p),
			data: data,
		})
	}
	return files
}

// fitArchive builds a zip archive that fits within maxSize, prioritizing log data.
func fitArchive(logData []byte, extras []extraFile, maxSize int64) ([]byte, error) {
	if len(logData) == 0 && len(extras) == 0 {
		return nil, nil
	}

	// Try everything.
	buf, err := writeArchive(logData, extras)
	if err != nil {
		return nil, err
	}
	if int64(buf.Len()) <= maxSize {
		return buf.Bytes(), nil
	}

	// Try full log, no extras.
	if len(logData) > 0 {
		buf, err = writeArchive(logData, nil)
		if err != nil {
			return nil, err
		}
		if int64(buf.Len()) <= maxSize {
			// Full log fits — greedily add extras that still fit.
			return addExtrasGreedily(logData, extras, maxSize)
		}

		// Full log doesn't fit — binary search for the maximum tail.
		tailSize := searchMaxLogTail(logData, maxSize)
		tail := logData[len(logData)-tailSize:]
		return addExtrasGreedily(tail, extras, maxSize)
	}

	// No log data — try extras only.
	return addExtrasGreedily(nil, extras, maxSize)
}

const logArchiveName = "lantern.log"

func writeArchive(logData []byte, extras []extraFile) (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	if len(logData) > 0 {
		fw, err := w.Create(logArchiveName)
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(logData); err != nil {
			return nil, err
		}
	}

	for _, f := range extras {
		fw, err := w.Create("attachments/" + f.name)
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(f.data); err != nil {
			return nil, err
		}
	}

	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf, nil
}

// searchMaxLogTail binary-searches for the largest tail of logData (in 256KB chunks)
// that compresses into a zip archive not exceeding maxSize.
func searchMaxLogTail(logData []byte, maxSize int64) int {
	const chunkSize = 256 * 1024
	n := len(logData)
	lo, hi := 1, (n+chunkSize-1)/chunkSize
	best := 0

	for lo <= hi {
		mid := lo + (hi-lo)/2
		tailBytes := mid * chunkSize
		if tailBytes > n {
			tailBytes = n
		}

		buf, err := writeArchive(logData[n-tailBytes:], nil)
		if err != nil {
			hi = mid - 1
			continue
		}
		if int64(buf.Len()) <= maxSize {
			best = tailBytes
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best
}

// addExtrasGreedily starts from the given log data and adds extra files one by one,
// keeping each only if the archive still fits within maxSize.
func addExtrasGreedily(logData []byte, extras []extraFile, maxSize int64) ([]byte, error) {
	var included []extraFile
	buf, err := writeArchive(logData, nil)
	if err != nil {
		return nil, err
	}
	lastGood := buf.Bytes()

	for _, f := range extras {
		// Safe append that won't modify the existing slice's backing array.
		trial := append(included[:len(included):len(included)], f)
		buf, err := writeArchive(logData, trial)
		if err != nil {
			continue
		}
		if int64(buf.Len()) <= maxSize {
			included = trial
			lastGood = buf.Bytes()
		}
	}
	return lastGood, nil
}
