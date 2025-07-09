package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/getlantern/common"
	"github.com/getlantern/radiance/app"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
)

// SetupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func SetupOTelSDK(ctx context.Context, cfg common.OTEL, contextDialer func(ctx context.Context, addr string) (net.Conn, error)) (func(context.Context) error, error) {
	var shutdownFuncs []func(context.Context) error
	if !cfg.TracesEnabled && !cfg.MetricsEnabled {
		// No need to initialize anything if all are disabled.
		return func(_ context.Context) error { return nil }, nil
	}
	var err error
	shutdown := func(ctx context.Context) error {
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}
	conn, err := initConn(cfg.Endpoint, contextDialer)
	if err != nil {
		return shutdown, fmt.Errorf("failed to initialize gRPC connection: %w", err)
	}
	shutdownFuncs = append(shutdownFuncs, func(_ context.Context) error { return conn.Close() })

	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "radiance"),
			attribute.String("library.language", "go"),
			attribute.String("service.version", app.Version),
		),
	)
	if err != nil {
		return shutdown, fmt.Errorf("failed to create resource: %w", err)
	}
	if cfg.TracesEnabled {
		tp, err := initTracer(ctx, res, conn, cfg)
		if err != nil {
			return shutdown, fmt.Errorf("failed to initialize tracer: %w", err)
		}
		// Successfully initialized tracer
		shutdownFuncs = append(shutdownFuncs, tp.Shutdown)
	}

	if cfg.MetricsEnabled {
		// Initialize the meter provider with the same gRPC connection.
		// This is useful to avoid creating multiple connections to the same endpoint.
		mp, err := initMeterProvider(ctx, res, conn, cfg)
		if err != nil {
			return shutdown, fmt.Errorf("failed to initialize meter provider: %w", err)
		}
		// Successfully initialized meter provider
		shutdownFuncs = append(shutdownFuncs, mp)
	}

	return shutdown, nil
}

// Initialize a gRPC connection to be used by both the tracer and meter
// providers.
func initConn(endpoint string, contextDialer func(ctx context.Context, addr string) (net.Conn, error)) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(endpoint,
		grpc.WithContextDialer(contextDialer),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection to collector: %w", err)
	}

	return conn, err
}

// initTracer creates and registers trace provider instance.
func initTracer(ctx context.Context, res *resource.Resource, conn *grpc.ClientConn, cfg common.OTEL) (*sdktrace.TracerProvider, error) {
	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithGRPCConn(conn),
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithHeaders(cfg.Headers),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	// Register the trace exporter with a TracerProvider, using a batch
	// span processor to aggregate spans before export.
	bsp := sdktrace.NewBatchSpanProcessor(exp)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TracesSampleRate))),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(tracerProvider)

	// Set global propagator to tracecontext (the default is no-op).
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tracerProvider, nil
}

// Initializes an OTLP exporter, and configures the corresponding meter provider.
func initMeterProvider(ctx context.Context, res *resource.Resource, conn *grpc.ClientConn, cfg common.OTEL) (func(context.Context) error, error) {
	metricExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics exporter: %w", err)
	}

	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
	}
	if cfg.MetricsInterval > 0 {
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter,
			sdkmetric.WithInterval(time.Duration(cfg.MetricsInterval)*time.Second))))
	} else {
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)))
	}
	meterProvider := sdkmetric.NewMeterProvider(
		opts...,
	)
	otel.SetMeterProvider(meterProvider)

	return meterProvider.Shutdown, nil
}
