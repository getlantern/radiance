package metrics

import (
	"context"
	"net"

	"github.com/sagernet/sing-box/adapter"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Conn wraps a net.Conn and tracks metrics such as bytes sent and received.
type Conn struct {
	net.Conn
	attributes           []attribute.KeyValue
	bytesSentCounter     metric.Int64Counter
	bytesReceivedCounter metric.Int64Counter
}

// NewConn creates a new Conn instance.
func NewConn(conn net.Conn, metadata *adapter.InboundContext) net.Conn {

	// Convert metadata to attributes
	attributes := []attribute.KeyValue{
		attribute.String("source_ip", metadata.Source.IPAddr().String()),
		attribute.String("protocol", metadata.Protocol),
		attribute.String("user", metadata.User),
		attribute.String("inbound", metadata.Inbound),
		attribute.String("outbound", metadata.Outbound),
		attribute.String("client", metadata.Client),
		attribute.String("domain", metadata.Domain),
	}

	meter := otel.GetMeterProvider().Meter("connection-monitor")

	bytesSentCounter, err := meter.Int64Counter("conn.bytes.sent", metric.WithDescription("Number of bytes sent on a connection"))
	if err != nil {
		return conn
	}

	bytesReceivedCounter, err := meter.Int64Counter("conn.bytes.received", metric.WithDescription("Number of bytes received on a connection"))
	if err != nil {
		return conn
	}

	return &Conn{
		Conn:                 conn,
		attributes:           attributes,
		bytesSentCounter:     bytesSentCounter,
		bytesReceivedCounter: bytesReceivedCounter,
	}
}

// Read overrides net.Conn's Read method to track received bytes.
func (c *Conn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		c.bytesReceivedCounter.Add(context.Background(), int64(n))
	}
	return
}

// Write overrides net.Conn's Write method to track sent bytes.
func (c *Conn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	if n > 0 {
		c.bytesSentCounter.Add(context.Background(), int64(n))
	}
	return
}
