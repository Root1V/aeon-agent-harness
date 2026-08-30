// Package tracing is OBS-001's shared OTel bootstrap: every Go service (run controller, model
// gateway, tool gateway) calls Init once at startup and gets back a Tracer that exports spans over
// OTLP/HTTP to the collector (deploy/compose/otel-collector-config.yaml), which forwards them to
// Tempo. Span names and attributes follow the OTel GenAI semantic conventions
// (invoke_agent/chat/execute_tool, gen_ai.*) — see docs/adr/0004 and proto/schemas/trace_event.schema.json.
//
// If Init is never called, otel.Tracer falls back to a global no-op provider — instrumented code
// stays safe to call from any binary or test, whether or not tracing is wired up.
package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Init configures the process-global TracerProvider to batch-export spans over OTLP/HTTP to
// endpoint (host:port, no scheme — e.g. "otel-collector:4318") tagged with serviceName, and
// returns a Tracer plus a shutdown func the caller should defer (flushes buffered spans and closes
// the exporter).
func Init(ctx context.Context, serviceName, endpoint string) (trace.Tracer, func(context.Context) error, error) {
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: building OTLP exporter for %s: %w", endpoint, err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
	))
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: building resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return tp.Tracer(serviceName), tp.Shutdown, nil
}
