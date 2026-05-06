package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/goccy/go-yaml"
)

// legacyYAMLCandidate looks for a pre-9.0.x flashlight/lantern-client
// settings file on disk and, if found, returns its translation into the
// current canonical JSON schema as a candidateSource. Returns the
// zero-value candidate when no pre-9.x file is present for this
// platform or when read/parse fails.
//
// Per-platform layout (only the platforms below have pre-9.x files
// readable from Go; Android stored its state in an encrypted SQLite
// and needs a Kotlin-side migration):
//
//	macOS:   ~/Library/Application Support/Lantern/settings.yaml  (desktop)
//	Windows: %APPDATA%\Lantern\settings.yaml                      (desktop)
//	Linux:   ~/.config/lantern/settings.yaml                      (desktop)
//	iOS:     <fileDir>/userconfig.yaml                            (ios)
//
// These match the path the pre-9.x flashlight + lantern-client used
// (getlantern/appdir.General("Lantern"), with the linux build tag
// lowercasing the app name).
//
// Field translation (desktop → canonical):
//
//	userID       (int64)  → user_id
//	deviceID     (string) → device_id
//	userPro      (bool)   → user_level ("pro" if true, "free" if id known)
//	userToken    (string) → token
//	emailAddress (string) → email
//
// Field translation (ios → canonical):
//
//	UserID   (int64)  → user_id
//	DeviceID (string) → device_id
//	Token    (string) → token
//
// (iOS didn't persist user_level in this YAML — pro state was kept in
// the Session proto and refreshed from the server. Leaving user_level
// unset means the next /account/login decides; user_id/device_id
// continuity is what we're after on iOS.)
// legacyYAMLPathFn is the function legacyYAMLCandidate uses to resolve
// the on-disk location of the pre-9.x YAML. Set as a package-level var
// (rather than calling legacyYAMLPath directly) so tests can redirect
// the lookup to a temp dir without mutating the host's
// ~/Library/Application Support / %APPDATA% / ~/.config layout.
var legacyYAMLPathFn = legacyYAMLPath

func legacyYAMLCandidate(fileDir string) candidateSource {
	path, layout := legacyYAMLPathFn(fileDir)
	if path == "" {
		return candidateSource{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("pre-9.x yaml read failed", "path", path, "error", err)
		}
		return candidateSource{}
	}
	translated, err := translateLegacyYAML(raw, layout)
	if err != nil {
		slog.Warn("pre-9.x yaml translate failed", "path", path, "error", err)
		return candidateSource{}
	}
	return candidateSource{
		path:     path,
		contents: translated,
		exists:   true,
		label:    fmt.Sprintf("pre-9.x %s yaml", layout),
	}
}

// legacyYAMLPath returns the on-disk location of the pre-9.x YAML
// settings file for this platform, plus the layout name we'll need to
// pick the right unmarshal struct. Returns ("", "") if this platform
// isn't supported here.
//
// Mirrors what getlantern/appdir.General("Lantern") produced in the
// pre-9.x desktop clients:
//   - darwin/windows: os.UserConfigDir() + "Lantern" (capitalized)
//   - linux:          os.UserConfigDir() + "lantern" (the linux build
//     tag in appdir lowercased the app name; pre-9.x clients on linux
//     wrote to ~/.config/lantern, not ~/.config/Lantern)
//
// On iOS the YAML lives inside the app sandbox alongside the radiance
// dataDir, so we look there directly.
func legacyYAMLPath(fileDir string) (path, layout string) {
	switch runtime.GOOS {
	case "darwin":
		// macOS desktop. iOS uses GOOS=ios in modern Go (>= 1.16) and
		// is handled separately below.
		if cfg, err := os.UserConfigDir(); err == nil {
			return filepath.Join(cfg, "Lantern", "settings.yaml"), "desktop"
		}
	case "windows":
		if cfg, err := os.UserConfigDir(); err == nil {
			return filepath.Join(cfg, "Lantern", "settings.yaml"), "desktop"
		}
	case "linux":
		// Linux pre-9.x lowercased the app name (see appdir_linux.go).
		if cfg, err := os.UserConfigDir(); err == nil {
			return filepath.Join(cfg, "lantern", "settings.yaml"), "desktop"
		}
	case "ios":
		// iOS lantern-client wrote userconfig.yaml inside the app
		// sandbox. The radiance dataDir on iOS is in the same sandbox.
		return filepath.Join(fileDir, "userconfig.yaml"), "ios"
	}
	return "", ""
}

// translateLegacyYAML parses the pre-9.x YAML and emits the equivalent
// canonical settings.json bytes. Unknown fields in the source are
// ignored; unset fields in the destination are omitted via omitempty.
func translateLegacyYAML(raw []byte, layout string) ([]byte, error) {
	type canonical struct {
		UserID    int64  `json:"user_id,omitempty"`
		DeviceID  string `json:"device_id,omitempty"`
		UserLevel string `json:"user_level,omitempty"`
		Token     string `json:"token,omitempty"`
		Email     string `json:"email,omitempty"`
	}

	var out canonical
	switch layout {
	case "desktop":
		var d struct {
			UserID       int64  `yaml:"userID"`
			DeviceID     string `yaml:"deviceID"`
			UserPro      bool   `yaml:"userPro"`
			UserToken    string `yaml:"userToken"`
			EmailAddress string `yaml:"emailAddress"`
		}
		if err := yaml.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("desktop yaml: %w", err)
		}
		out.UserID = d.UserID
		out.DeviceID = d.DeviceID
		out.Token = d.UserToken
		out.Email = d.EmailAddress
		switch {
		case d.UserPro:
			out.UserLevel = "pro"
		case d.UserID != 0:
			// User identified but not pro — write "free" so downstream
			// code sees a real value instead of an empty string.
			out.UserLevel = "free"
		}
	case "ios":
		var i struct {
			UserID   int64  `yaml:"UserID"`
			DeviceID string `yaml:"DeviceID"`
			Token    string `yaml:"Token"`
		}
		if err := yaml.Unmarshal(raw, &i); err != nil {
			return nil, fmt.Errorf("ios yaml: %w", err)
		}
		out.UserID = i.UserID
		out.DeviceID = i.DeviceID
		out.Token = i.Token
		// user_level intentionally left empty — iOS didn't persist it
		// in this YAML, and an empty value lets the next /account/login
		// be authoritative.
	default:
		return nil, fmt.Errorf("unknown layout: %s", layout)
	}
	return json.Marshal(out)
}
