// Package env is responsible for loading radiance configuration based on a order of precedence
// (environment variables > configurations set at .env file).
package env

import (
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type _key string

var (
	LogLevel         _key = "RADIANCE_LOG_LEVEL"
	LogPath          _key = "RADIANCE_LOG_PATH"
	DataPath         _key = "RADIANCE_DATA_PATH"
	DisableFetch     _key = "RADIANCE_DISABLE_FETCH_CONFIG"
	PrintCurl        _key = "RADIANCE_PRINT_CURL"
	DisableStdout    _key = "RADIANCE_DISABLE_STDOUT_LOG"
	ENV              _key = "RADIANCE_ENV"
	UseSocks         _key = "RADIANCE_USE_SOCKS_PROXY"
	SocksAddress     _key = "RADIANCE_SOCKS_ADDRESS"
	Country          _key = "RADIANCE_COUNTRY"
	FeatureOverrides _key = "RADIANCE_FEATURE_OVERRIDES"
	AppVersion       _key = "RADIANCE_VERSION"

	Testing _key = "RADIANCE_TESTING"

	mu     sync.RWMutex
	dotenv = map[string]string{}
)

func (k _key) String() string { return string(k) }

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
				dotenv[key] = value
			}
		}
	}
	if testing.Testing() {
		dotenv[Testing.String()] = "true"
		dotenv[LogLevel.String()] = "disable"
	}
}

// LoadFromDir reads a .env file from the given directory and merges its values
// into the dotenv map. Values from this call override any previously loaded from
// the working-directory .env (from init). This is needed on Android where the
// app's working directory is "/" but the data directory (where the test harness
// pushes .env) is elsewhere (e.g. /data/user/0/org.getlantern.lantern/.lantern).
func LoadFromDir(dir string) {
	if dir == "" {
		return
	}
	path := filepath.Join(dir, ".env")
	buf, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		slog.Error(".env found in data dir but failed to read", slog.String("path", path), slog.Any("error", err))
		return
	}
	slog.Info("Loaded .env from data directory", slog.String("path", path))
	lines := strings.SplitSeq(string(buf), "\n")
	for line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			dotenv[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
}

func Get(key _key) (string, bool) {
	mu.RLock()
	value, exists := dotenv[key.String()]
	mu.RUnlock()
	if exists {
		return value, true
	}
	if value, exists := os.LookupEnv(key.String()); exists {
		return value, true
	}
	return "", false
}

// Set sets a key in the in-memory dotenv map, overriding any .env file or OS
// environment variable value. This is intended for dev/testing use via IPC.
func Set(key string, value string) {
	mu.Lock()
	dotenv[key] = value
	mu.Unlock()
}

// GetAll returns a copy of the in-memory dotenv map.
func GetAll() map[string]string {
	mu.RLock()
	defer mu.RUnlock()
	m := make(map[string]string, len(dotenv))
	maps.Copy(m, dotenv)
	return m
}

func GetString(key _key) string {
	value, _ := Get(key)
	return value
}

func GetBool(key _key) bool {
	value, exists := Get(key)
	if !exists {
		return false
	}
	v, _ := strconv.ParseBool(value)
	return v
}

func GetInt(key _key) int {
	value, exists := Get(key)
	if !exists {
		return 0
	}
	v, _ := strconv.Atoi(value)
	return v
}

func SetStagingEnv() {
	slog.Info("setting environment to staging for testing")
	Set(ENV.String(), "staging")
	Set(PrintCurl.String(), "true")
}
