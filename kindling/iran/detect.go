// Package iran provides device-local detection of whether the user is
// currently in Iran. Designed to run without any network access since
// the Lantern API may be unreachable exactly when the heuristic is
// needed. Classification is imperfect; callers should expose a manual
// override.
package iran

import "time"

// IranMCC is the ITU-T E.212 Mobile Country Code for Iran.
const IranMCC = "432"

// IranTZName is the IANA timezone identifier for Iran.
const IranTZName = "Asia/Tehran"

// LikelyIran reports whether on-device signals suggest the user is in
// Iran. When mcc is non-empty it is authoritative and overrides
// tzName; when empty (WiFi-only, no signal, iOS 16+) the function
// falls back to tzName alone.
func LikelyIran(mcc, tzName string) bool {
	if mcc != "" {
		return mcc == IranMCC
	}
	return tzName == IranTZName
}

// LocalTZName returns the process's system timezone in IANA name
// form, or "" when no zone could be determined.
func LocalTZName() string {
	if time.Local == nil {
		return ""
	}
	return time.Local.String()
}
