package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	_ "unsafe" // for go:linkname

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
)

// this endpoint is used exclusively to set the data path on Linux running under systemd and Windows
// running as a service since we don't know the user's home directory ahead of time and/or we start
// before the UI process which is responsible for setting the data path in settings.
// This is temporary and will be removed once we move ownership and interaction of all files to
// one process.

type setOption struct {
	SettingsPath string `json:"settings_path"`
}

// Deprecated: This is temporary and will be removed in the future. This should only be used on
// Linux running under systemd or as a service on Windows.
// SetSettingsPath sets the data path for lanternd and reloads settings and should be called as soon
// as possible on startup to ensure all logs are written to the correct location.
func SetSettingsPath(ctx context.Context, path string) error {
	_, err := sendRequest[empty](ctx, "POST", setSettingsPathEndpoint, setOption{SettingsPath: path})
	return err
}

func (s *Server) setSettingsPathHandler(w http.ResponseWriter, r *http.Request) {
	var opt setOption
	err := json.NewDecoder(r.Body).Decode(&opt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	old := settings.GetString(settings.DataPathKey)
	slog.Debug("Received request to update data path", "new", opt.SettingsPath, "old", old)

	path := opt.SettingsPath
	name := filepath.Base(settings.GetString("file_path"))
	if filepath.Base(path) != name {
		path = filepath.Join(path, name)
	}
	if err := settings.Set(settings.DataPathKey, path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := reloadSettings(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := reinitLogger(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Debug("Updated data path", "new", path)
	settings.SetReadOnly(true)
	w.WriteHeader(http.StatusOK)
}

func reinitLogger() error {
	path := filepath.Join(settings.GetString(settings.LogPathKey), common.LogFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	lvl, _ := internal.ParseLogLevel(settings.GetString(settings.LogLevelKey))
	slog.SetDefault(internal.NewLogger(f, lvl))
	return nil
}

//go:linkname reloadSettings github.com/getlantern/radiance/common/settings.loadSettings
func reloadSettings(path string) error
