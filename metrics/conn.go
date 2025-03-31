package metrics

import (
	"context"
	"net"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Conn wraps a net.Conn and tracks metrics such as bytes sent and received.
type Conn struct {
	net.Conn
	attributes []attribute.KeyValue
	startTime  time.Time
}

// NewConn creates a new Conn instance.
func NewConn(conn net.Conn, metadata *adapter.InboundContext) net.Conn {
	// Convert metadata to attributes
	attributes := []attribute.KeyValue{
		attribute.String("proxy_ip", metadata.Destination.IPAddr().String()),
		attribute.String("protocol", metadata.Protocol),
		attribute.String("user", metadata.User),
		attribute.String("inbound", metadata.Inbound),
		attribute.String("outbound", metadata.Outbound),
		attribute.String("client", metadata.Client),
		attribute.String("domain", metadata.Domain),
	}

	return &Conn{
		Conn:       conn,
		attributes: attributes,
		startTime:  time.Now(),
	}
}

// Read overrides net.Conn's Read method to track received bytes.
func (c *Conn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		metrics.bytesReceived.Add(context.Background(), int64(n), metric.WithAttributes(c.attributes...))
	}
	return
}

// Write overrides net.Conn's Write method to track sent bytes.
func (c *Conn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	if n > 0 {
		metrics.bytesReceived.Add(context.Background(), int64(n), metric.WithAttributes(c.attributes...))
	}
	return
}

// Close overrides net.Conn's Close method to track connection duration.
func (c *Conn) Close() error {
	duration := time.Since(c.startTime).Milliseconds()
	metrics.duration.Record(context.Background(), duration, metric.WithAttributes(c.attributes...))
	metrics.conns.Add(context.Background(), -1, metric.WithAttributes(c.attributes...))
	return c.Conn.Close()
}
