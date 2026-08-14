package issue

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/getlantern/radiance/common"
)

const (
	// maxLogReadFactor bounds how much uncompressed log to read toward an archive:
	// maxArchiveSize * maxLogReadFactor bytes. Logs compress by at most roughly this
	// factor, so reading more than this can never be needed to reach the compressed
	// size budget. That is a compression bound, not a memory one — see
	// maxMobileInputBytes.
	maxLogReadFactor int64 = 20
	// maxMobileInputBytes caps the uncompressed bytes the archiver holds at once on
	// mobile. maxLogReadFactor alone permits 19.5 MB * 20 = 390 MB per file, no
	// constraint at all inside the iOS network extension's fatal 50 MB jetsam cap:
	// reporting an issue while connected read 42 MB of logs and the extension was
	// killed mid-archive, dropping the tunnel. engineering#3820.
	maxMobileInputBytes int64 = 8 * 1024 * 1024
	// backupTimeFormat matches the timestamp format used in rotated backup
	// filenames: "<log-name>-<timestamp>.log.gz".
	backupTimeFormat = "2006-01-02T15-04-05.000"

	logExt    = ".log"
	backupExt = ".log.gz"
)

// buildIssueArchive creates a zip archive containing all .log files found in
// logDir plus additional attachment files. The primary log (lantern.log) is
// given truncation priority; secondary log files and attachments are included
// greedily if space permits. The total compressed archive size will not exceed
// maxSize bytes.
func buildIssueArchive(logDir string, additionalFiles []string, maxSize int64) ([]byte, error) {
	logFiles := globFiles(logDir, "*.log")

	var primaryLogData []byte
	var secondaryLogs []extraFile

	// One budget shared across every read below, so the archiver's peak is bounded
	// by the platform rather than by the sum of whatever happens to be on disk.
	// Reading the tail means a smaller budget costs history, not correctness.
	budget := archiveInputBudget(maxSize)
	for _, lf := range logFiles {
		if budget <= 0 {
			slog.Warn("archive input budget exhausted, skipping log", "path", lf)
			continue
		}
		data, err := snapshotLogFile(lf, budget)
		if err != nil {
			slog.Warn("unable to snapshot log file", "path", lf, "error", err)
			continue
		}
		if len(data) == 0 {
			continue
		}
		budget -= int64(len(data))
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
	for _, a := range attachments {
		budget -= int64(len(a.data))
	}

	primaryPath := filepath.Join(logDir, logArchiveName)
	primaryLogData = prependMostRecentBackup(primaryPath, primaryLogData, budget)

	return fitArchive(primaryLogData, secondaryLogs, attachments, maxSize)
}

// archiveInputBudget is the total uncompressed bytes the archiver may hold. On
// mobile the process ceiling matters more than archive completeness.
func archiveInputBudget(maxSize int64) int64 {
	budget := maxSize * maxLogReadFactor
	if common.IsMobile() && budget > maxMobileInputBytes {
		return maxMobileInputBytes
	}
	return budget
}

// prependMostRecentBackup prepends the newest rotated gzip backup, if present,
// so a mid-session rotation does not lose earlier log history.
// remaining is what is left of the archiver's shared input budget; the backup is
// decompressed into memory, so it has to draw from the same pool as the live logs.
func prependMostRecentBackup(primaryLogPath string, current []byte, remaining int64) []byte {
	backupPath, ok := findMostRecentCompressedBackup(primaryLogPath)
	if !ok {
		return current
	}

	if remaining <= 0 {
		return current
	}

	backupData, err := readGzipTail(backupPath, remaining)
	if err != nil {
		slog.Warn("unable to read compressed log backup", "path", backupPath, "error", err)
		return current
	}
	if len(backupData) == 0 {
		return current
	}
	if backupData[len(backupData)-1] != '\n' {
		backupData = append(backupData, '\n')
	}

	return append(backupData, current...)
}

func findMostRecentCompressedBackup(primaryLogPath string) (string, bool) {
	dir := filepath.Dir(primaryLogPath)
	base := filepath.Base(primaryLogPath)
	logName := strings.TrimSuffix(base, logExt)

	matches := globFiles(dir, logName+"-*"+backupExt)
	if len(matches) == 0 {
		return "", false
	}

	var latestPath string
	var latestTimestamp time.Time
	for _, match := range matches {
		timestamp, ok := parseBackupTimestamp(match, logName)
		if ok && timestamp.After(latestTimestamp) {
			latestTimestamp = timestamp
			latestPath = match
		}
	}

	return latestPath, latestPath != ""
}

func parseBackupTimestamp(path, logName string) (time.Time, bool) {
	filename := filepath.Base(path)
	prefix := logName + "-"
	if !strings.HasPrefix(filename, prefix) || !strings.HasSuffix(filename, backupExt) {
		return time.Time{}, false
	}

	timestamp := filename[len(prefix) : len(filename)-len(backupExt)]
	ts, err := time.Parse(backupTimeFormat, timestamp)
	return ts, err == nil
}

// readGzipTail returns up to maxRead bytes from the end of the decompressed file.
// The tail is kept rather than the head because it is the history nearest in time
// to the current log; memory stays bounded to ~maxRead even when the backup
// decompresses to more.
func readGzipTail(path string, maxRead int64) ([]byte, error) {
	if maxRead <= 0 {
		return nil, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	var tail []byte
	chunk := make([]byte, 32*1024)
	for {
		n, readErr := gz.Read(chunk)
		if n > 0 {
			tail = append(tail, chunk[:n]...)
			if int64(len(tail)) > maxRead {
				tail = append(tail[:0], tail[int64(len(tail))-maxRead:]...)
			}
		}
		if readErr == io.EOF {
			return tail, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func globFiles(dir, pattern string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		slog.Warn("unable to glob files", "dir", dir, "pattern", pattern, "error", err)
		return nil
	}
	return matches
}

// snapshotLogFile opens the log file, records its current size, and reads at most
// maxRead bytes of the tail.
func snapshotLogFile(logPath string, maxRead int64) ([]byte, error) {
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
