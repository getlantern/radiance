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

// buildIssueArchive creates a zip archive containing all .log files found in
// logDir plus additional attachment files. The primary log (lantern.log) is
// given truncation priority; secondary log files and attachments are included
// greedily if space permits. The total compressed archive size will not exceed
// maxSize bytes.
func buildIssueArchive(logDir string, additionalFiles []string, maxSize int64) ([]byte, error) {
	logFiles := globLogFiles(logDir)

	var primaryLogData []byte
	var secondaryLogs []extraFile

	for _, lf := range logFiles {
		data, err := snapshotLogFile(lf, maxSize)
		if err != nil {
			slog.Warn("unable to snapshot log file", "path", lf, "error", err)
			continue
		}
		if len(data) == 0 {
			continue
		}
		if filepath.Base(lf) == logArchiveName {
			primaryLogData = data
		} else {
			secondaryLogs = append(secondaryLogs, extraFile{
				name: filepath.Base(lf),
				data: data,
			})
		}
	}

	attachments := readExtraFiles(additionalFiles)

	return fitArchive(primaryLogData, secondaryLogs, attachments, maxSize)
}

// globLogFiles returns all .log files in dir, sorted by filepath.Glob order.
func globLogFiles(dir string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		slog.Warn("unable to glob log files", "dir", dir, "error", err)
		return nil
	}
	return matches
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

// fitArchive builds a zip archive that fits within maxSize. The primary log
// (lantern.log) is given truncation priority, followed by secondary log files,
// then attachments.
func fitArchive(primaryLog []byte, secondaryLogs []extraFile, attachments []extraFile, maxSize int64) ([]byte, error) {
	allLogs := logsFromPrimary(primaryLog, secondaryLogs)

	if len(allLogs) == 0 && len(attachments) == 0 {
		return nil, nil
	}

	// Try everything.
	buf, err := writeArchive(allLogs, attachments)
	if err != nil {
		return nil, err
	}
	if int64(buf.Len()) <= maxSize {
		return buf.Bytes(), nil
	}

	// Try primary log only.
	primaryLogs := logsFromPrimary(primaryLog, nil)
	if len(primaryLog) > 0 {
		buf, err = writeArchive(primaryLogs, nil)
		if err != nil {
			return nil, err
		}
		if int64(buf.Len()) <= maxSize {
			// Full primary fits — greedily add secondary logs, then attachments.
			return addExtrasGreedily(primaryLogs, secondaryLogs, attachments, maxSize)
		}

		// Full primary doesn't fit — binary search for the maximum tail.
		tailSize := searchMaxLogTail(primaryLog, maxSize)
		tail := primaryLog[len(primaryLog)-tailSize:]
		trimmedPrimary := logsFromPrimary(tail, nil)
		return addExtrasGreedily(trimmedPrimary, secondaryLogs, attachments, maxSize)
	}

	// No primary log — greedily add secondary logs and attachments.
	return addExtrasGreedily(nil, secondaryLogs, attachments, maxSize)
}

// logsFromPrimary builds a combined log entry list with the primary log first.
func logsFromPrimary(primaryLog []byte, secondaryLogs []extraFile) []extraFile {
	var logs []extraFile
	if len(primaryLog) > 0 {
		logs = append(logs, extraFile{name: logArchiveName, data: primaryLog})
	}
	logs = append(logs, secondaryLogs...)
	return logs
}

const logArchiveName = "lantern.log"

func writeArchive(logs []extraFile, attachments []extraFile) (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	for _, l := range logs {
		fw, err := w.Create(l.name)
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(l.data); err != nil {
			return nil, err
		}
	}

	for _, f := range attachments {
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

		logs := []extraFile{{name: logArchiveName, data: logData[n-tailBytes:]}}
		buf, err := writeArchive(logs, nil)
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

// addExtrasGreedily starts from the given base logs and greedily adds secondary
// log files then attachment files, keeping each only if the archive still fits
// within maxSize.
func addExtrasGreedily(baseLogs []extraFile, secondaryLogs []extraFile, attachments []extraFile, maxSize int64) ([]byte, error) {
	currentLogs := make([]extraFile, len(baseLogs))
	copy(currentLogs, baseLogs)
	var currentAttachments []extraFile

	buf, err := writeArchive(currentLogs, nil)
	if err != nil {
		return nil, err
	}
	lastGood := buf.Bytes()

	// Greedily add secondary log files.
	for _, sl := range secondaryLogs {
		trial := append(currentLogs[:len(currentLogs):len(currentLogs)], sl)
		buf, err := writeArchive(trial, currentAttachments)
		if err != nil {
			continue
		}
		if int64(buf.Len()) <= maxSize {
			currentLogs = trial
			lastGood = buf.Bytes()
		}
	}

	// Greedily add attachment files.
	for _, a := range attachments {
		trial := append(currentAttachments[:len(currentAttachments):len(currentAttachments)], a)
		buf, err := writeArchive(currentLogs, trial)
		if err != nil {
			continue
		}
		if int64(buf.Len()) <= maxSize {
			currentAttachments = trial
			lastGood = buf.Bytes()
		}
	}

	return lastGood, nil
}
