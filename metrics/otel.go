// Package metrics provides OpenTelemetry setup for radiance
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/getlantern/common"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"google.golang.org/grpc/credentials"
)

type Attributes struct {
	App            string
	AppVersion     string
	DeviceID       string
	GeoCountry     string
	GoVersion      string
	LocaleLanguage string
	LocaleCountry  string
	Platform       string
	OSName         string
	OSArch         string
	OSVersion      string
	Timezone       string
	Pro            bool
}

func buildResources(serviceName string, a Attributes) []attribute.KeyValue {
	return []attribute.KeyValue{
		semconv.ServiceNameKey.String(serviceName),
		semconv.ServiceVersionKey.String(a.AppVersion),
		attribute.String("device.id", a.DeviceID),
		attribute.String("geo.country", a.GeoCountry),
		attribute.String("library.language", "go"),
		attribute.String("library.language.version", a.GoVersion),
		attribute.String("locale.language", a.LocaleLanguage),
		attribute.String("locale.country", a.LocaleCountry),
		attribute.String("platform", a.Platform),
		attribute.String("os.name", a.OSName),
		attribute.String("os.arch", a.OSArch),
		attribute.String("os.version", a.OSVersion),
		attribute.String("timezone", a.Timezone),
		attribute.Bool("is_pro", a.Pro),
	}
}

// SetupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func SetupOTelSDK(ctx context.Context, attributes Attributes, cfg *common.ConfigResponse) (func(context.Context) error, error) {
	if cfg.Features == nil {
		cfg.Features = make(map[string]bool)
	}
	val, ok := cfg.Features[common.TRACES]
	tracesEnabled := ok && val
	val, ok = cfg.Features[common.METRICS]
	metricsEnabled := ok && val
	if !tracesEnabled && !metricsEnabled {
		// No need to initialize anything if all are disabled.
		return func(_ context.Context) error { return nil }, nil
	}
	var shutdownFuncs []func(context.Context) error
	var err error
	shutdown := func(ctx context.Context) error {
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}
	serviceName := "radiance"
	res, err := resource.New(ctx,
		resource.WithAttributes(
			buildResources(serviceName, attributes)...,
		),
	)
	if err != nil {
		return shutdown, fmt.Errorf("failed to create resource: %w", err)
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if tracesEnabled {
		shutdownFunc, err := initTracer(ctx, res, cfg.OTEL)
		if err != nil {
			return shutdown, fmt.Errorf("failed to initialize tracer: %w", err)
		}
		// Successfully initialized tracer
		shutdownFuncs = append(shutdownFuncs, shutdownFunc)
		slog.Info("OpenTelemetry tracer initialized")
	}

	if metricsEnabled {
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
