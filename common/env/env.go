package env

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strings"
)

type Key = string

const (
	LogLevel     Key = "RADIANCE_LOG_LEVEL"
	LogPath      Key = "RADIANCE_LOG_PATH"
	DataPath     Key = "RADIANCE_DATA_PATH"
	DisableFetch Key = "RADIANCE_DISABLE_FETCH_CONFIG"
)

var (
	envVars = map[string]any{}
)

func init() {
	buf, err := os.ReadFile(".env")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error(".env file found, but failed to read", slog.Any("error", err))
	} else if err == nil {
		// Parse the .env file and populate envVars
		lines := strings.SplitSeq(string(buf), "\n")
		for line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue // Skip empty lines and comments
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				envVars[key] = value
			}
		}
	}

	// Check for environment variables and populate envVars, overriding any values from the .env file
	for _, key := range []string{LogLevel, LogPath, DataPath, DisableFetch} {
		if value, exists := os.LookupEnv(key); exists {
			envVars[key] = value
		}
	}
}

func Get[T any](key Key) (T, bool) {
	if value, exists := envVars[key]; exists {
		if v, ok := value.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}
