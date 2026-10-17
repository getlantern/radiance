package settings

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// windowsCrossDirCandidatesFn is overridable so tests can redirect the
// lookup to a temp dir without touching the host's %PUBLIC%.
var windowsCrossDirCandidatesFn = windowsCrossDirCandidates

// windowsCrossDirCandidates returns the v9.0.x local.json from ${PUBLIC}\Lantern\data, which the
// same-dir candidates miss because radiance's data dir moved to ${ProgramData}\Lantern.
func windowsCrossDirCandidates(fileDir string) []candidateSource {
	if runtime.GOOS != "windows" {
		return nil
	}
	pub := os.Getenv("PUBLIC")
	if pub == "" {
		return nil
	}
	v90xDir := filepath.Join(pub, "Lantern", "data")
	// If the caller's fileDir already IS the v9.0.x dir (e.g. someone
	// manually pointed lanternd at ${PUBLIC}\Lantern\data), the same-dir
	// candidates already cover it — no need to also list it here.
	// NTFS is case-insensitive by default, so a caller that supplies an
	// equivalent path with different casing or separators should also
	// hit this guard.
	if samePathWindows(fileDir, v90xDir) {
		return nil
	}
	return readWindowsCrossDirCandidates(v90xDir)
}

// samePathWindows reports whether two paths refer to the same directory
// on Windows. Cleans both sides (collapsing `.` / `..` and normalizing
// separators) and compares case-insensitively because NTFS is
// case-insensitive by default. Not a substitute for filepath.EvalSymlinks
// or device-id comparison — it only handles the common "user passed the
// same dir written differently" case, which is the only one the
// duplicate-scan guard needs to defend against.
func samePathWindows(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// readWindowsCrossDirCandidates is split out so tests can drive the path
// resolution directly without needing to spoof %PUBLIC% / GOOS.
func readWindowsCrossDirCandidates(v90xDir string) []candidateSource {
	// settings.json is deliberately not read: in this directory it is the live UI's settings, not a
	// v9.0.x artifact, so adopting it would import whatever the UI last wrote.
	full := filepath.Join(v90xDir, legacySettingsFileName)
	label := "v9.0.x Windows " + full
	b, err := os.ReadFile(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		slog.Warn("legacy settings migration: read failed", "path", full, "error", err)
		return nil
	}
	return []candidateSource{{
		path:     full,
		contents: b,
		exists:   true,
		label:    label,
	}}
}
