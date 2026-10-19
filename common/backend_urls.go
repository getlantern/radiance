//go:build !lantern_e2e

package common

// GetProServerURL returns the Pro API endpoint for the selected environment.
func GetProServerURL() string {
	if Stage() {
		return StageProServerURL
	}
	return ProServerURL
}

// GetBaseURL returns the account API endpoint for the selected environment.
func GetBaseURL() string {
	if Stage() {
		return StageBaseURL
	}
	return BaseURL
}
