package app

const (
	AppName = "radiance"

	// Placeholders to use in the request headers.
	ClientVersion = "7.6.47"
	Version       = "7.6.47"

	DeviceId = "some-uuid-here"

	// userId and proToken will be set to actual values when user management is implemented.
	// set to specific value so the server returns a desired config.
	// - 23409 -> shadowsocks
	// - 23403 -> algeneva
	UserId   = "23403"
	ProToken = ""

	// TODO: will be used by the vpn-client branch, and later should be user configurable
	//  this is here because of the need to attach log to the issue report
	LogDir = "logs"
)
