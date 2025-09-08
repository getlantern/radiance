package ipc

import (
	"encoding/json"
	"net/http"
	runtimeDebug "runtime/debug"
	"time"

	"github.com/getlantern/radiance/traces"
	"github.com/gofrs/uuid/v5"
	"github.com/sagernet/sing-box/common/conntrack"
	"github.com/sagernet/sing-box/experimental/clashapi/trafficontrol"
	"go.opentelemetry.io/otel"
)

// CloseConnections closes connections by their IDs. If connIDs is empty, all connections will be closed.
func CloseConnections(connIDs []string) error {
	_, err := sendRequest[empty]("POST", closeConnectionsEndpoint, connIDs)
	return err
}

func (s *Server) closeConnectionHandler(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer(tracerName).Start(r.Context(), "server.closeConnectionHandler")
	defer span.End()
	if s.service.Status() != StatusRunning {
		http.Error(w, traces.RecordError(span, errServiceIsNotReady).Error(), http.StatusServiceUnavailable)
		return
	}
	var cids []string
	err := json.NewDecoder(r.Body).Decode(&cids)
	if err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusBadRequest)
		return
	}
	if len(cids) > 0 {
		tm := s.service.ClashServer().TrafficManager()
		for _, cid := range cids {
			targetConn := tm.Connection(uuid.FromStringOrNil(cid))
			if targetConn == nil {
				continue
			}
			targetConn.Close()
		}
	} else {
		conntrack.Close()
	}
	go func() {
		time.Sleep(time.Second)
		runtimeDebug.FreeOSMemory()
	}()
	w.WriteHeader(http.StatusOK)
}

// GetConnections retrieves the list of current and recently closed connections.
func GetConnections() ([]Connection, error) {
	return sendRequest[[]Connection]("GET", connectionsEndpoint, nil)
}

func (s *Server) connectionsHandler(w http.ResponseWriter, r *http.Request) {
	_, span := otel.Tracer(tracerName).Start(r.Context(), "server.connectionsHandler")
	defer span.End()

	if s.service.Status() != StatusRunning {
		http.Error(w, traces.RecordError(span, errServiceIsNotReady).Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	tm := s.service.ClashServer().TrafficManager()
	activeConns := tm.Connections()
	closedConns := tm.ClosedConnections()
	connections := make([]Connection, 0, len(activeConns)+len(closedConns))
	for _, connection := range activeConns {
		connections = append(connections, newConnection(connection))
	}
	for _, connection := range closedConns {
		connections = append(connections, newConnection(connection))
	}
	if err := json.NewEncoder(w).Encode(connections); err != nil {
		http.Error(w, traces.RecordError(span, err).Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Connection represents a network connection with relevant metadata.
type Connection struct {
	ID           string
	Inbound      string
	IPVersion    int
	Network      string
	Source       string
	Destination  string
	Domain       string
	Protocol     string
	FromOutbound string
	CreatedAt    int64
	ClosedAt     int64
	Uplink       int64
	Downlink     int64
	Rule         string
	Outbound     string
	ChainList    []string
}

func newConnection(metadata trafficontrol.TrackerMetadata) Connection {
	var rule string
	if metadata.Rule != nil {
		rule = metadata.Rule.String() + " => " + metadata.Rule.Action().String()
	}
	var closedAt int64
	if !metadata.ClosedAt.IsZero() {
		closedAt = metadata.ClosedAt.UnixMilli()
	}
	md := metadata.Metadata
	return Connection{
		ID:           metadata.ID.String(),
		Inbound:      md.InboundType + "/" + md.Inbound,
		IPVersion:    int(md.IPVersion),
		Network:      md.Network,
		Source:       md.Source.String(),
		Destination:  md.Destination.String(),
		Domain:       md.Domain,
		Protocol:     md.Protocol,
		FromOutbound: md.Outbound,
		CreatedAt:    metadata.CreatedAt.UnixMilli(),
		ClosedAt:     closedAt,
		Uplink:       metadata.Upload.Load(),
		Downlink:     metadata.Download.Load(),
		Rule:         rule,
		Outbound:     metadata.OutboundType + "/" + metadata.Outbound,
		ChainList:    metadata.Chain,
	}
}
