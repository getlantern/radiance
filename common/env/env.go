// Package env is responsible for loading radiance configuration based on a order of precedence
// (environment variables > configurations set at .env file).
package env

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/getlantern/radiance/internal"
)

type Key = string

const (
	LogLevel      Key = "RADIANCE_LOG_LEVEL"
	LogPath       Key = "RADIANCE_LOG_PATH"
	DataPath      Key = "RADIANCE_DATA_PATH"
	DisableFetch  Key = "RADIANCE_DISABLE_FETCH_CONFIG"
	PrintCurl     Key = "RADIANCE_PRINT_CURL"
	DisableStdout Key = "RADIANCE_DISABLE_STDOUT_LOG"
	ENV           Key = "RADIANCE_ENV"

	Testing Key = "RADIANCE_TESTING"
)

var (
	keys = []Key{
		LogLevel,
		LogPath,
		DataPath,
		DisableFetch,
		PrintCurl,
		DisableStdout,
		ENV,
	}
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
				parseAndSet(key, value)
			}
		}
	}

	// Check for environment variables and populate envVars, overriding any values from the .env file
	for _, key := range keys {
		if value, exists := os.LookupEnv(key); exists {
			parseAndSet(key, value)
		}
	}
	if testing.Testing() {
		envVars[Testing] = true
		envVars[LogLevel] = "DISABLE"
		slog.SetLogLoggerLevel(internal.Disable)
	}
}

// Get retrieves the value associated with the given key and attempts to cast it to type T. If the
// key does not exist or the type does not match, it returns the zero value of T and false.
func Get[T any](key Key) (T, bool) {
	if value, exists := envVars[key]; exists {
		if v, ok := value.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

func parseAndSet(key, value string) {
	// Attempt to parse as a boolean
	if b, err := strconv.ParseBool(value); err == nil {
		envVars[key] = b
		return
	}
	// Attempt to parse as an integer
	if i, err := strconv.Atoi(value); err == nil {
		envVars[key] = i
		return
	}
	// Otherwise, store as a string
	envVars[key] = value
}
