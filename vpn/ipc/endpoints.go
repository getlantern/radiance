package ipc

const (
	statusEndpoint           = "/status"
	metricsEndpoint          = "/metrics"
	startServiceEndpoint     = "/service/start"
	closeServiceEndpoint     = "/service/close"
	groupsEndpoint           = "/groups"
	selectEndpoint           = "/outbound/select"
	activeEndpoint           = "/outbound/active"
	clashModeEndpoint        = "/clash/mode"
	connectionsEndpoint      = "/connections"
	closeConnectionsEndpoint = "/connections/close"
)
