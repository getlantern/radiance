package common

import "context"

// OauthResult holds the outcome of the OAuth flow.
type OauthResult struct {
	Token string
	Err   error
}

// OauthSession represents an ongoing OAuth authentication flow.
type OauthSession struct {
	// Result is a channel that will receive exactly one value: the access token on success,
	// or an error if the process fails or is cancelled.
	Result <-chan OauthResult
	// CancelFunc can be called to abort the authentication flow.
	Cancel context.CancelFunc
}
