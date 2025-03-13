package issue

// copy from flashlight/logging/logging.go
// this should be moved to logging specific package

import (
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/getlantern/radiance/util"
)

type fileInfo struct {
	file    string
	size    int64
	modTime int64
}
type byDate []*fileInfo

func (a byDate) Len() int           { return len(a) }
func (a byDate) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byDate) Less(i, j int) bool { return a[i].modTime > a[j].modTime }

// zipLogFiles zips the Lantern log files to the writer. All files will be
// placed under the folder in the archieve.  It will stop and return if the
// newly added file would make the extracted files exceed maxBytes in total.
//
// It also returns up to maxTextBytes of plain text from the end of the most recent log file.
func zipLogFiles(w io.Writer, logDir string, maxBytes int64, maxTextBytes int64) (string, error) {
	return zipLogFilesFrom(w, maxBytes, maxTextBytes, map[string]string{"logs": logDir})
}

// zipLogFilesFrom zips the log files from the given dirs to the writer. It will
// stop and return if the newly added file would make the extracted files exceed
// maxBytes in total.
//
// It also returns up to maxTextBytes of plain text from the end of the most recent log file.
func zipLogFilesFrom(w io.Writer, maxBytes int64, maxTextBytes int64, dirs map[string]string) (string, error) {
	globs := make(map[string]string, len(dirs))
	for baseDir, dir := range dirs {
		globs[baseDir] = filepath.Join(dir, "*")
	}
	err := util.ZipFiles(w, util.ZipOptions{
		Globs:    globs,
		MaxBytes: maxBytes,
	})
	if err != nil {
		return "", err
	}

	if maxTextBytes <= 0 {
		return "", nil
	}

	// Get info for all log files
	allFiles := make(byDate, 0)
	for _, glob := range globs {
		matched, err := filepath.Glob(glob)
		if err != nil {
			log.Errorf("Unable to list files at glob %v: %v", glob, err)
			continue
		}
		for _, file := range matched {
			fi, err := os.Stat(file)
			if err != nil {
				log.Errorf("Unable to stat file %v: %v", file, err)
				continue
			}
			allFiles = append(allFiles, &fileInfo{
				file:    file,
				size:    fi.Size(),
				modTime: fi.ModTime().Unix(),
			})
		}
	}

	if len(allFiles) > 0 {
		// Sort by recency
		sort.Sort(allFiles)

		mostRecent := allFiles[0]
		log.Debugf("Grabbing log tail from %v", mostRecent.file)

		mostRecentFile, err := os.Open(mostRecent.file)
		if err != nil {
			log.Errorf("Unable to open most recent log file %v: %v", mostRecent.file, err)
			return "", nil
		}
		defer mostRecentFile.Close()

		seekTo := mostRecent.size - maxTextBytes
		if seekTo > 0 {
			log.Debugf("Seeking to %d in %v", seekTo, mostRecent.file)
			_, err = mostRecentFile.Seek(seekTo, io.SeekCurrent)
			if err != nil {
				log.Errorf("Unable to seek to tail of file %v: %v", mostRecent.file, err)
				return "", nil
			}
		}
		tail, err := io.ReadAll(mostRecentFile)
		if err != nil {
			log.Errorf("Unable to read tail of file %v: %v", mostRecent.file, err)
			return "", nil
		}

		log.Debugf("Got %d bytes of log tail from %v", len(tail), mostRecent.file)
		return string(tail), nil
	}

	return "", nil
}
