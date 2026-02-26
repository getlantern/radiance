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
	clashModeEndpoint        = "/clash/mode"
	connectionsEndpoint      = "/connections"
	closeConnectionsEndpoint = "/connections/close"
	setSettingsPathEndpoint  = "/set"
	statusEventsEndpoint     = "/status/events"
)
