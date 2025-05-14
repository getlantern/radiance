package common

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/getlantern/appdir"

	"github.com/getlantern/radiance/app"
)

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
	return
}

func maybeAddSuffix(path, suffix string) string {
	if filepath.Base(path) != suffix {
		path = filepath.Join(path, suffix)
	}
	return path
}
