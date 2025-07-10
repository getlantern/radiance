package metrics

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"time"

	"github.com/getlantern/common"
	"github.com/getlantern/radiance/app"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/credentials"
)

// SetupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func SetupOTelSDK(ctx context.Context, cfg common.OTEL) (func(context.Context) error, error) {
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
		shutdownFunc, err := initTracer(res, cfg)
		if err != nil {
			return shutdown, fmt.Errorf("failed to initialize tracer: %w", err)
		}
		// Successfully initialized tracer
		shutdownFuncs = append(shutdownFuncs, shutdownFunc)
		slog.Info("OpenTelemetry tracer initialized")
	}

	if cfg.MetricsEnabled {
		// Initialize the meter provider with the same gRPC connection.
		// This is useful to avoid creating multiple connections to the same endpoint.
		mp, err := initMeterProvider(ctx, res, cfg)
		if err != nil {
			return shutdown, fmt.Errorf("failed to initialize meter provider: %w", err)
		}
		// Successfully initialized meter provider
		shutdownFuncs = append(shutdownFuncs, mp)
	}

	return shutdown, nil
}

func initTracer(res *resource.Resource, cfg common.OTEL) (func(context.Context) error, error) {
	exporter, err := otlptrace.New(
		context.Background(),
		otlptracegrpc.NewClient(
			otlptracegrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, "")),
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
			otlptracegrpc.WithHeaders(cfg.Headers),
		),
	)
	if err != nil {
		log.Fatalf("Failed to create exporter: %v", err)
		return nil, fmt.Errorf("failed to create exporter: %w", err)
	}

	otel.SetTracerProvider(
		sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TracesSampleRate))),
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(res),
		),
	)
	return exporter.Shutdown, nil
}

// Initializes an OTLP exporter, and configures the corresponding meter provider.
func initMeterProvider(ctx context.Context, res *resource.Resource, cfg common.OTEL) (func(context.Context) error, error) {
	metricExporter, err := otlpmetricgrpc.New(ctx) //, otlpmetricgrpc.WithGRPCConn(conn))
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
