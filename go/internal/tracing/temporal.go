package tracing

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"
)

// TemporalInterceptor is the client-side half of "a run is one trace" (OBS-010b).
//
// WHAT WAS BROKEN, and Argus found it from their own store before we did: `OBS-010` installed
// Temporal's OTel interceptor on the PYTHON side, so a run's Activity spans stopped being eight
// unrelated roots. The first hop stayed loose — `aeon-runcontroller` dialled Temporal with no
// interceptors at all, so the HTTP request that starts a run never put its trace context into the
// workflow's history and the worker's trace began at the worker. Their words: "lo que todavía no veo
// es una traza que cruce del runcontroller al worker pasando por la espera". There was nothing to see.
//
// WHY THE PROPAGATOR IS NOT PASSED, which is the one decision here worth writing down. Their
// `TracerOptions.TextMapPropagator` documents that it defaults to their own
// `DefaultTextMapPropagator` and NOT to the OpenTelemetry global one — exactly the kind of
// almost-right default this project keeps finding. It happens to be the same composite this package
// installs globally for the HTTP hop (W3C TraceContext + Baggage), so there is nothing to pass and
// passing it would make things worse: `otel.GetTextMapPropagator()` returns a NO-OP until someone
// registers one (the OBS-006b finding), so a binary that dialled Temporal before Init would capture
// that no-op for the life of the process and propagate nothing, silently.
//
// That agreement is a coincidence of two libraries' defaults, not a contract, so
// TestTemporalAndHTTPHopsCarryTheSameFormats asserts it rather than trusting it.
//
// The Worker side lives in the Python worker (aeon_worker/__main__.py) because that is where the
// workflows are; this is only the client that starts, signals and queries them.
func TemporalInterceptor() interceptor.ClientInterceptor {
	i, err := opentelemetry.NewTracingInterceptor(opentelemetry.TracerOptions{
		// Errors reading a parent span from headers are swallowed rather than failing the call.
		// Runs started before this existed have no `_tracer-data` header at all, and their signals,
		// queries and cancellations must keep working — an observability change that refuses to
		// cancel an in-flight run is the OBS-010 lesson again, one layer up.
		AllowInvalidParentSpans: true,
	})
	if err != nil {
		// NewTracingInterceptor only errors on a malformed option set, and the one above is a literal.
		// Returning nil would be an interceptor list with a nil in it, which panics at dial.
		panic("tracing: constructing the Temporal interceptor: " + err.Error())
	}
	return i
}

// TemporalPropagator is what the Temporal hop actually uses, exported for the test that checks it
// against the HTTP one. Reading it from their package rather than restating it is the point: a
// restated copy would agree with itself forever.
func TemporalPropagator() propagation.TextMapPropagator {
	return opentelemetry.DefaultTextMapPropagator
}

// Propagator is what the HTTP hop uses: the process-global one, which Init registers. Read from the
// global rather than restated, so the test that compares the two hops compares what is installed and
// not what this file believes is installed.
func Propagator() propagation.TextMapPropagator {
	return otel.GetTextMapPropagator()
}
