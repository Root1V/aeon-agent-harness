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
	"os"

	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// DefaultNamespace is the application every Aeon service belongs to. Overridable by ARGUS_NAMESPACE so a
// deployment running two Aeon installations can tell them apart, which is the level their incident routing
// keys on.
const DefaultNamespace = "aeon-ai"

func namespace() string {
	if ns := os.Getenv("ARGUS_NAMESPACE"); ns != "" {
		return ns
	}
	return DefaultNamespace
}

// Init configures the process-global TracerProvider to batch-export spans over OTLP/HTTP to
// endpoint (host:port, no scheme — e.g. "otel-collector:4318") tagged with serviceName, and
// returns a Tracer plus a shutdown func the caller should defer (flushes buffered spans and closes
// the exporter).
func Init(ctx context.Context, serviceName, role, endpoint string) (trace.Tracer, func(context.Context) error, error) {
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: building OTLP exporter for %s: %w", endpoint, err)
	}

	// THREE ATTRIBUTES AND NOT ONE, and Argus found out why by looking at their own incident board.
	//
	// We were sending only service.name. Their gateway fills a missing service.namespace with
	// `unregistered`, deliberately — a service that emits without declaring itself still shows up — but an
	// `unregistered` application enters with medium criticality AND NO NOTIFICATION CHANNELS, so its alerts
	// go to their own console log. Our policy denial from the day before was sitting there as
	// `unregistered / aeon-toolgw`, detected correctly and delivered to nobody.
	//
	//   service.namespace      the APPLICATION            aeon-ai
	//   service.name           the SUB-COMPONENT          toolgw, worker, modelgw, ...
	//   argus.component.role   from their closed set      api | worker | scheduler | cli | model-server
	//
	// The component name drops the `aeon-` prefix: with the namespace present it repeats what the namespace
	// already says. That redundancy is exactly what their own check caught in our test client, which was
	// sending service.namespace equal to service.name and so collapsing two levels of identity into one.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceNamespace(namespace()),
		attribute.String("argus.component.role", role),
	))
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: building resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// THE PROPAGATOR, which was missing and is the reason OBS-001 produced spans that reached Tempo
	// without ever forming a trace.
	//
	// OTel Go's default global propagator is a NO-OP. So every incoming request extracted nothing and
	// every outgoing call injected nothing: each service created ROOT spans, and one Deep Research run
	// appeared as half a dozen unrelated traces. OBS-001's test never caught it because it queried the
	// three spans SEPARATELY — `name = "chat"`, `name = "execute_tool"`, `name = "invoke_agent"` — and
	// each existed. The assertion was "these spans exist", which is weaker than "this is one trace", and
	// the gap between the two is exactly what an incident needs.
	//
	// W3C traceparent plus baggage, because that is what the Python side emits and what any OTel
	// collector — including an Argus agent — expects.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Tracer(serviceName), tp.Shutdown, nil
}
