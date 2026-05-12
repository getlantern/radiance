package settings

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// windowsCrossDirCandidatesFn is overridable so tests can redirect the
// lookup to a temp dir without touching the host's %PUBLIC%.
var windowsCrossDirCandidatesFn = windowsCrossDirCandidates

// windowsCrossDirCandidates returns settings candidates from the v9.0.x
// Windows data directory (${PUBLIC}\Lantern\data), which is no longer
// scanned by the same-dir migration after PR #370 moved the lanternd
// daemon to ${ProgramData}\Lantern.
//
// On v9.0.x Windows, radiance was embedded in the Flutter app via FFI
// and used the Dart-supplied data dir at C:\Users\Public\Lantern\data
// (see lantern/lib/core/utils/storage_utils.dart). PR #370 (the same
// commit that introduced cmd/lanternd/lanternd_windows.go) split radiance
// off into a standalone Windows service that reads/writes under
// internal.DefaultDataPath() = ${ProgramData}\Lantern. The two directories
// don't share a parent, so the same-dir candidates in
// migrateLegacySettingsIfNeeded never see the v9.0.x file and the user's
// Pro state is lost on upgrade. See getlantern/engineering#3460 and
// Freshdesk #174606.
//
// Returns nil on non-Windows hosts or when %PUBLIC% is unset. Both
// known v9.0.x filenames are tried (local.json — the original — and
// settings.json — what the file was renamed to in PR #370 for users who
// upgraded through an intermediate build) so the recovery works
// regardless of which v9.0.x release the user is coming from.
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
	if filepath.Clean(fileDir) == filepath.Clean(v90xDir) {
		return nil
	}
	return readWindowsCrossDirCandidates(v90xDir)
}

// readWindowsCrossDirCandidates is split out so tests can drive the path
// resolution directly without needing to spoof %PUBLIC% / GOOS.
func readWindowsCrossDirCandidates(v90xDir string) []candidateSource {
	// legacySettingsFileName is tried first because it's the actual
	// v9.0.x name; settingsFileName is included as a defensive
	// fallback for users whose v9.0.x file got renamed by a partial /
	// failed earlier upgrade attempt.
	specs := []struct {
		name, label string
	}{
		{legacySettingsFileName, fmt.Sprintf("v9.0.x Windows %s", filepath.Join(v90xDir, legacySettingsFileName))},
		{settingsFileName, fmt.Sprintf("v9.0.x Windows %s", filepath.Join(v90xDir, settingsFileName))},
	}
	var out []candidateSource
	for _, s := range specs {
		full := filepath.Join(v90xDir, s.name)
		b, err := os.ReadFile(full)
		switch {
		case err == nil:
			out = append(out, candidateSource{
				path:     full,
				contents: b,
				exists:   true,
				label:    s.label,
			})
		case errors.Is(err, fs.ErrNotExist):
			// Expected — fresh install or that filename never existed for this user.
		default:
			slog.Warn("v9.0.x Windows cross-dir read failed",
				"path", full, "error", err)
		}
	}
	return out
}
