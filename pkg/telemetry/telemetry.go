package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellogapi "go.opentelemetry.io/otel/log"
	otellogglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

type Shutdown func(context.Context) error

type BootstrapConfig struct {
	Endpoint        string
	ServiceName     string
	ServiceVersion  string
	Environment     string
	MetricsInterval time.Duration
	Level           slog.Leveler
	Insecure        bool
}

type otelRuntime struct {
	meterProvider  *metric.MeterProvider
	traceProvider  *sdktrace.TracerProvider
	loggerProvider *sdklog.LoggerProvider

	shutdownOnce sync.Once
	shutdownErr  error
}

func Bootstrap(ctx context.Context, cfg BootstrapConfig) (*slog.Logger, Shutdown, error) {
	if cfg.Level == nil {
		cfg.Level = slog.LevelInfo
	}
	baseHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.Level})
	baseLogger := slog.New(baseHandler)

	if strings.TrimSpace(cfg.ServiceName) == "" {
		cfg.ServiceName = "rwct-agent"
	}
	if strings.TrimSpace(cfg.ServiceVersion) == "" {
		cfg.ServiceVersion = "dev"
	}
	if strings.TrimSpace(cfg.Environment) == "" {
		cfg.Environment = "local"
	}
	if cfg.MetricsInterval <= 0 {
		cfg.MetricsInterval = 10 * time.Second
	}
	if !cfg.Insecure {
		cfg.Insecure = true
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return baseLogger.With("service", cfg.ServiceName), func(context.Context) error { return nil }, nil
	}

	runtime, err := initOpenTelemetry(ctx, cfg)
	if err != nil {
		return baseLogger.With("service", cfg.ServiceName), func(context.Context) error { return nil }, err
	}
	otelHandler := otelslog.NewHandler(cfg.ServiceName,
		otelslog.WithLoggerProvider(runtime.loggerProvider),
		otelslog.WithVersion(cfg.ServiceVersion),
		otelslog.WithAttributes(
			attribute.String(string(semconv.DeploymentEnvironmentNameKey), cfg.Environment),
		),
	)
	logger := slog.New(&fanoutHandler{
		handlers: []slog.Handler{
			baseHandler,
			otelHandler,
		},
	}).With("service", cfg.ServiceName)

	return logger, runtime.Shutdown, nil
}

func initOpenTelemetry(ctx context.Context, cfg BootstrapConfig) (*otelRuntime, error) {
	endpoint, insecure, err := normalizeEndpoint(cfg.Endpoint, cfg.Insecure)
	if err != nil {
		return nil, err
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
		semconv.DeploymentEnvironmentName(cfg.Environment),
	)

	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(endpoint)}
	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
	logOpts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(endpoint)}
	if insecure {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		logOpts = append(logOpts, otlploggrpc.WithInsecure())
	}

	metricExp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		return nil, err
	}
	traceExp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, err
	}
	logExp, err := otlploggrpc.New(ctx, logOpts...)
	if err != nil {
		return nil, err
	}

	mp := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(metricExp, metric.WithInterval(cfg.MetricsInterval))),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
	)
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)

	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)
	otellogglobal.SetLoggerProvider(lp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &otelRuntime{
		meterProvider:  mp,
		traceProvider:  tp,
		loggerProvider: lp,
	}, nil
}

func (r *otelRuntime) Shutdown(ctx context.Context) error {
	r.shutdownOnce.Do(func() {
		var errs []error
		if r.loggerProvider != nil {
			if err := r.loggerProvider.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		if r.traceProvider != nil {
			if err := r.traceProvider.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		if r.meterProvider != nil {
			if err := r.meterProvider.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		r.shutdownErr = errors.Join(errs...)
	})
	return r.shutdownErr
}

func normalizeEndpoint(raw string, fallbackInsecure bool) (string, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fallbackInsecure, fmt.Errorf("empty OTEL endpoint")
	}
	if !strings.Contains(raw, "://") {
		return raw, fallbackInsecure, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fallbackInsecure, err
	}
	if strings.TrimSpace(u.Host) == "" {
		return "", fallbackInsecure, fmt.Errorf("invalid OTEL endpoint: %q", raw)
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "http":
		return u.Host, true, nil
	case "https":
		return u.Host, false, nil
	default:
		return "", fallbackInsecure, fmt.Errorf("unsupported OTEL endpoint scheme %q", u.Scheme)
	}
}

func WrapHTTPHandler(handler http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(handler, operation)
}

func NewHTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}

func InjectTraceContext(ctx context.Context) (traceParent, traceState string) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return strings.TrimSpace(carrier.Get("traceparent")), strings.TrimSpace(carrier.Get("tracestate"))
}

func ExtractTraceContext(ctx context.Context, traceParent, traceState string) context.Context {
	carrier := propagation.MapCarrier{}
	traceParent = strings.TrimSpace(traceParent)
	traceState = strings.TrimSpace(traceState)
	if traceParent != "" {
		carrier.Set("traceparent", traceParent)
	}
	if traceState != "" {
		carrier.Set("tracestate", traceState)
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

func LoggerProvider() otellogapi.LoggerProvider {
	return otellogglobal.GetLoggerProvider()
}

func InitMetrics(ctx context.Context, endpoint string) (Shutdown, error) {
	cfg := BootstrapConfig{
		Endpoint:        endpoint,
		ServiceName:     "rwct-agent",
		ServiceVersion:  "dev",
		Environment:     "local",
		MetricsInterval: 10 * time.Second,
		Level:           slog.LevelInfo,
		Insecure:        true,
	}
	runtime, err := initOpenTelemetry(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return runtime.Shutdown, nil
}

type fanoutHandler struct {
	handlers []slog.Handler
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, item := range h.handlers {
		if item.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *fanoutHandler) Handle(ctx context.Context, rec slog.Record) error {
	var errs []error
	for _, item := range h.handlers {
		if !item.Enabled(ctx, rec.Level) {
			continue
		}
		if err := item.Handle(ctx, rec.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, 0, len(h.handlers))
	for _, item := range h.handlers {
		next = append(next, item.WithAttrs(attrs))
	}
	return &fanoutHandler{handlers: next}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, 0, len(h.handlers))
	for _, item := range h.handlers {
		next = append(next, item.WithGroup(name))
	}
	return &fanoutHandler{handlers: next}
}
