package common

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/getlantern/appdir"

	"github.com/getlantern/radiance/app"
)

var (
	dataPath atomic.Value
	logPath  atomic.Value
)

// ensure dataPath and logPath are of type string
func init() {
	dataPath.Store("")
	logPath.Store("")
}

func SetupDirectories(data, logs string) (dataDir, logDir string, err error) {
	dataDir = data
	if dataDir == "" {
		dataDir = appdir.General(app.Name)
	}
	logDir = logs
	if logDir == "" {
		logDir = appdir.Logs(app.Name)
	}
	dataDir = maybeAddSuffix(dataDir, "data")
	logDir = maybeAddSuffix(logDir, "logs")
	for _, path := range []string{dataDir, logDir} {
		if err := os.MkdirAll(path, 0755); err != nil {
			return "", "", fmt.Errorf("failed to create directory %s: %w", path, err)
		}
	}

	dataPath.Store(dataDir)
	logPath.Store(logDir)
	return
}

func maybeAddSuffix(path, suffix string) string {
	if filepath.Base(path) != suffix {
		path = filepath.Join(path, suffix)
	}
	return path
}

func DataPath() string {
	return dataPath.Load().(string)
}

func LogPath() string {
	return logPath.Load().(string)
}
