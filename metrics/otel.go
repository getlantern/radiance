package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/getlantern/common"
	rcommon "github.com/getlantern/radiance/common"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"google.golang.org/grpc/credentials"
)

// SetupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func SetupOTelSDK(ctx context.Context, cfg common.ConfigResponse) (func(context.Context) error, error) {
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
			semconv.ServiceName("radiance"),
			semconv.ServiceVersion(rcommon.Version),
			attribute.String("library.language", "go"),
			attribute.String("platform", rcommon.Platform),
			attribute.Bool("pro", cfg.UserInfo.Pro),
		),
	)
	if err != nil {
		return shutdown, fmt.Errorf("failed to create resource: %w", err)
	}

	if cfg.TracesEnabled {
		shutdownFunc, err := initTracer(ctx, res, cfg.OTEL)
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
		mp, err := initMeterProvider(ctx, res, cfg.OTEL)
		if err != nil {
			return shutdown, fmt.Errorf("failed to initialize meter provider: %w", err)
		}
		// Successfully initialized meter provider
		shutdownFuncs = append(shutdownFuncs, mp)
	}

	return shutdown, nil
}

func initTracer(ctx context.Context, res *resource.Resource, cfg common.OTEL) (func(context.Context) error, error) {
	exporter, err := otlptrace.New(
		ctx,
		otlptracegrpc.NewClient(
			otlptracegrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, "")),
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
			otlptracegrpc.WithHeaders(cfg.Headers),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create exporter: %w", err)
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TracesSampleRate))),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tracerProvider)
	return func(ctx context.Context) error {
		if err := tracerProvider.Shutdown(ctx); err != nil {
			return fmt.Errorf("failed to shutdown tracer provider: %w", err)
		}
		if err := exporter.Shutdown(ctx); err != nil {
			return fmt.Errorf("failed to shutdown exporter: %w", err)
		}
		return nil
	}, nil
}

// Initializes an OTLP exporter, and configures the corresponding meter provider.
func initMeterProvider(ctx context.Context, res *resource.Resource, cfg common.OTEL) (func(context.Context) error, error) {
	metricExporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, "")),
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
		otlpmetricgrpc.WithHeaders(cfg.Headers),
	)
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
