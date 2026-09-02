package api

import (
	"context"
	"os"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// ensureTestTracing initializes a real OTel pipeline exactly once for this whole test binary,
// shared by every test in this package that needs one (TestDistributedTracingSpansReachTempo,
// TestAgentConsoleShowsARunEndToEnd, ...). This exists because of a real, reproducible flakiness
// found while adding the second such test: each test previously called tracing.Init (which replaces
// the process-global TracerProvider) and then called that provider's own Shutdown to force an
// immediate flush before querying Tempo. Shutting down a TracerProvider is terminal — a package-
// level tracer obtained elsewhere in this codebase (e.g. modelgateway's, runcontroller's own
// otel.Tracer(...) call at package-init time) that had already resolved against the now-shut-down
// provider stayed bound to a closed exporter for the rest of the process, so the *next* test's
// spans silently never reached the collector. A single shared provider, flushed (never shut down)
// via ForceFlush after each test, avoids this entirely.
func ensureTestTracing(t *testing.T) (flush func(context.Context) error) {
	t.Helper()
	endpoint := os.Getenv("AEON_TEST_OTEL_ENDPOINT")
	if endpoint == "" {
		t.Skip("AEON_TEST_OTEL_ENDPOINT not set — skipping tracing integration test (see make test-go-integration)")
	}

	tracingSetupOnce.Do(func() {
		tracingSetupErr = initSharedTestTracerProvider(endpoint)
	})
	if tracingSetupErr != nil {
		t.Fatalf("initializing the shared test TracerProvider: %v", tracingSetupErr)
	}
	return sharedTestTracerProvider.ForceFlush
}

var (
	tracingSetupOnce         sync.Once
	tracingSetupErr          error
	sharedTestTracerProvider *sdktrace.TracerProvider
)

func initSharedTestTracerProvider(endpoint string) error {
	exp, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return err
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("aeon-api-integration-test"),
	))
	if err != nil {
		return err
	}
	sharedTestTracerProvider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(sharedTestTracerProvider)
	return nil
}
