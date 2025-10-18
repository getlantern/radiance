// Package telemetry provides OpenTelemetry setup for radiance
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/getlantern/common"
	"github.com/getlantern/osversion"

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

	rcommon "github.com/getlantern/radiance/common"
)

var (
	initMutex                   sync.Mutex
	shutdownOTEL                func(context.Context) error
	harvestConnections          sync.Once
	harvestConnectionTickerStop func()
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

// OnNewConfig handles OpenTelemetry re-initialization when the configuration changes.
func OnNewConfig(oldConfig, newConfig common.ConfigResponse, deviceID string, userInfo rcommon.UserInfo) error {
	// Check if the old OTEL configuration is the same as the new one.
	if reflect.DeepEqual(oldConfig.OTEL, newConfig.OTEL) {
		slog.Debug("OpenTelemetry configuration has not changed, skipping initialization")
		return nil
	}
	userData, err := userInfo.GetData()
	if err != nil {
		slog.Error("Failed to get user data for OpenTelemetry initialization", "error", err)
		return nil
	}
	if err := initialize(deviceID, newConfig, userData.LegacyUserData.UserLevel == "pro"); err != nil {
		slog.Error("Failed to initialize OpenTelemetry", "error", err)
		return nil
	}
	return nil
}

func initialize(deviceID string, configResponse common.ConfigResponse, pro bool) error {
	initMutex.Lock()
	defer initMutex.Unlock()

	if configResponse.OTEL.Endpoint == "" {
		slog.Debug("OpenTelemetry configuration has not changed, skipping initialization")
		return nil
	}

	if shutdownOTEL != nil {
		slog.Info("Shutting down existing OpenTelemetry SDK")
		if err := shutdownOTEL(context.Background()); err != nil {
			slog.Error("Failed to shutdown OpenTelemetry SDK", "error", err)
			return fmt.Errorf("failed to shutdown OpenTelemetry SDK: %w", err)
		}
		shutdownOTEL = nil
	}

	attrs := Attributes{
		App:        "radiance",
		DeviceID:   deviceID,
		AppVersion: rcommon.ClientVersion,
		Platform:   rcommon.Platform,
		GoVersion:  runtime.Version(),
		OSName:     runtime.GOOS,
		OSArch:     runtime.GOARCH,
		GeoCountry: configResponse.Country,
		Pro:        pro,
	}
	if osStr, err := osversion.GetHumanReadable(); err == nil {
		attrs.OSVersion = osStr
	}

	shutdown, err := setupOTelSDK(context.Background(), attrs, configResponse)
	if err != nil {
		slog.Error("Failed to start OpenTelemetry SDK", "error", err)
		return fmt.Errorf("failed to start OpenTelemetry SDK: %w", err)
	}

	shutdownOTEL = shutdown

	harvestConnections.Do(func() {
		harvestConnectionTickerStop = harvestConnectionMetrics(1 * time.Minute)
	})
	return nil
}

func Close(ctx context.Context) error {
	initMutex.Lock()
	defer initMutex.Unlock()

	var errs error

	// stop collecting connection metrics
	if harvestConnectionTickerStop != nil {
		harvestConnectionTickerStop()
	}

	if shutdownOTEL != nil {
		slog.Info("Shutting down existing OpenTelemetry SDK")
		if err := shutdownOTEL(ctx); err != nil {
			slog.Error("Failed to shutdown OpenTelemetry SDK", "error", err)
			errs = errors.Join(errs, fmt.Errorf("failed to shutdown OpenTelemetry SDK: %w", err))
		}
		shutdownOTEL = nil
	}
	return errs
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

// setupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func setupOTelSDK(ctx context.Context, attributes Attributes, cfg common.ConfigResponse) (func(context.Context) error, error) {
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
