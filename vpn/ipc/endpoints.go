package ipc

const (
	statusEndpoint           = "/status"
	metricsEndpoint          = "/metrics"
	startServiceEndpoint     = "/service/start"
	stopServiceEndpoint      = "/service/stop"
	restartServiceEndpoint   = "/service/restart"
	groupsEndpoint           = "/groups"
	selectEndpoint           = "/outbound/select"
	activeEndpoint           = "/outbound/active"
	updateOutboundsEndpoint  = "/outbound/update"
	addOutboundsEndpoint     = "/outbound/add"
	removeOutboundsEndpoint  = "/outbound/remove"
	clashModeEndpoint        = "/clash/mode"
	connectionsEndpoint      = "/connections"
	closeConnectionsEndpoint = "/connections/close"
	setSettingsPathEndpoint  = "/set"
)
