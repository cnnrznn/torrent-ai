package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// instrumentationName names the tracer, meter and logger this program
// reports under.
const instrumentationName = "github.com/cnnrznn/torrent-ai"

// otelConfig holds the -otel flags. Each non-empty value is written to
// the standard OTEL_* variable it corresponds to, so flags (and their
// TORRENT_AI_OTEL_* variables) take precedence over OTEL_* already in
// the environment, which in turn apply when a flag is left empty.
type otelConfig struct {
	enabled bool

	endpoint        string
	protocol        string
	headers         string
	insecure        string
	caFile          string
	tracesExporter  string
	metricsExporter string
	logsExporter    string
	serviceName     string
	resourceAttrs   string
	sampleRatio     string
	metricInterval  time.Duration
}

// env returns the OTEL_* variables the flags set.
func (c otelConfig) env() map[string]string {
	env := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			env[k] = v
		}
	}
	set("OTEL_EXPORTER_OTLP_ENDPOINT", c.endpoint)
	set("OTEL_EXPORTER_OTLP_PROTOCOL", c.protocol)
	set("OTEL_EXPORTER_OTLP_HEADERS", c.headers)
	set("OTEL_EXPORTER_OTLP_INSECURE", c.insecure)
	set("OTEL_EXPORTER_OTLP_CERTIFICATE", c.caFile)
	set("OTEL_TRACES_EXPORTER", c.tracesExporter)
	set("OTEL_METRICS_EXPORTER", c.metricsExporter)
	set("OTEL_LOGS_EXPORTER", c.logsExporter)
	set("OTEL_SERVICE_NAME", c.serviceName)
	set("OTEL_RESOURCE_ATTRIBUTES", c.resourceAttrs)
	if c.sampleRatio != "" {
		env["OTEL_TRACES_SAMPLER"] = "parentbased_traceidratio"
		env["OTEL_TRACES_SAMPLER_ARG"] = c.sampleRatio
	}
	if c.metricInterval > 0 {
		env["OTEL_METRIC_EXPORT_INTERVAL"] = strconv.FormatInt(c.metricInterval.Milliseconds(), 10)
	}
	return env
}

func (c otelConfig) validate() error {
	if c.sampleRatio != "" {
		r, err := strconv.ParseFloat(c.sampleRatio, 64)
		if err != nil || r < 0 || r > 1 {
			return fmt.Errorf("-otel-sample-ratio must be a number from 0 to 1, got %q", c.sampleRatio)
		}
	}
	switch c.protocol {
	case "", "grpc", "http/protobuf":
	default:
		return fmt.Errorf("-otel-protocol must be grpc or http/protobuf, got %q", c.protocol)
	}
	return nil
}

// telemetry is the running OpenTelemetry SDK, or a no-op when -otel is
// off.
type telemetry struct {
	// logHandler sends slog records to the OTel log pipeline; nil when
	// logs aren't exported.
	logHandler slog.Handler
	shutdown   func(context.Context) error
}

// setupTelemetry starts the OTel SDK for traces, metrics and logs and
// installs it globally. Exporters are chosen by the standard OTEL_*
// variables (see -h), so with -otel off nothing is set up and the
// global providers stay no-ops.
func setupTelemetry(ctx context.Context, c otelConfig) (*telemetry, error) {
	t := &telemetry{shutdown: func(context.Context) error { return nil }}
	if !c.enabled {
		return t, nil
	}

	for k, v := range c.env() {
		os.Setenv(k, v)
	}
	if os.Getenv("OTEL_SERVICE_NAME") == "" {
		os.Setenv("OTEL_SERVICE_NAME", "torrent-ai")
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithProcessPID(),
		resource.WithAttributes(attribute.String("service.version", version())),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	spans, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("otel traces: %w", err)
	}
	reader, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("otel metrics: %w", err)
	}
	logs, err := autoexport.NewLogExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("otel logs: %w", err)
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(spans), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(logs)), sdklog.WithResource(res))

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	global.SetLoggerProvider(lp)

	if !autoexport.IsNoneLogExporter(logs) {
		t.logHandler = otelslog.NewHandler(instrumentationName, otelslog.WithLoggerProvider(lp))
	}
	t.shutdown = func(ctx context.Context) error {
		// Spans first, so the run's root span is exported, then the
		// final metrics and the logs describing the shutdown.
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}
	return t, nil
}

// version returns the module version the binary was built from.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}
