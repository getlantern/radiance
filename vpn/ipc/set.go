package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"

	_ "unsafe" // for go:linkname

	"github.com/getlantern/radiance/common/settings"
)

// this endpoint is used exclusively to set the data path on Linux since we're running under systemd
// and we don't know the user's home directory ahead of time.
// This is temporary and will be removed once we move ownership and interaction of all files to
// one process. maybe daemon?

var errLinuxOnly = errors.New("setting data path is only supported on Linux")

type setOption struct {
	SettingsPath string `json:"settings_path"`
}

func SetSettingsPath(ctx context.Context, path string) error {
	if runtime.GOOS != "linux" {
		return errLinuxOnly
	}
	_, err := sendRequest[empty](ctx, "POST", setSettingsPathEndpoint, setOption{SettingsPath: path})
	return err
}

func (s *Server) setSettingsPathHandler(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "linux" {
		http.Error(w, errLinuxOnly.Error(), http.StatusBadRequest)
		return
	}
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
	slog.Debug("Updated data path", "new", path)
	settings.SetReadOnly(true)
	w.WriteHeader(http.StatusOK)
}

//go:linkname reloadSettings github.com/getlantern/radiance/common/settings.loadSettings
func reloadSettings(path string) error
