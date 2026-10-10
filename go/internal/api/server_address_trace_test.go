package api

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/providers"
)

// addressedFakeProvider is renamingFakeProvider plus providers.ServerAddresser. It builds its answer
// through providers.HostOf rather than returning a literal, so the real extraction path — the one
// that strips credentials and ports — is what the span attribute comes from.
type addressedFakeProvider struct {
	renamingFakeProvider
	endpoint string
}

func (p addressedFakeProvider) ServerAddress() string { return providers.HostOf(p.endpoint) }

// TestServerAddressIsOnTheSpan is VRT-AXO-002's `server.address`, against real Tempo.
//
// It exists because of a gap in our own delivery rather than a new request: the convention agreed
// with Argus fixes five things, we shipped three, and bundled this attribute with the span RENAME —
// then gated both on Argus telling us when a rename would be safe for their dashboards. The gate is
// real for the rename and never applied to an additive attribute, so an agreed attribute stayed
// unbuilt while the channel already said it "goes on the span".
func TestServerAddressIsOnTheSpan(t *testing.T) {
	tempoURL := os.Getenv("AEON_TEST_TEMPO_QUERY_URL")
	if tempoURL == "" {
		t.Skip("AEON_TEST_TEMPO_QUERY_URL not set — skipping tracing integration test (see make test-go-integration)")
	}
	flush := ensureTestTracing(t)
	ctx := context.Background()

	// EVERY QUERY BELOW IS SCOPED TO A UNIQUE MARKER. Tempo keeps what previous runs sent it, so an
	// unscoped query reads another run's data — including a run of a previous version of this code,
	// which is exactly how the response-model test passed locally on a flag that had been removed.
	host := fmt.Sprintf("obs014-host-%d.test", time.Now().UnixNano())
	addressed := fmt.Sprintf("obs014-addressed-%d", time.Now().UnixNano())
	silent := fmt.Sprintf("obs014-silent-%d", time.Now().UnixNano())

	gw := modelgateway.New()
	gw.RegisterProvider("addressed", addressedFakeProvider{
		renamingFakeProvider: renamingFakeProvider{served: addressed},
		// Userinfo and a port, both of which must be gone from the attribute. A credential in
		// telemetry cannot be walked back once collectors have it.
		endpoint: "https://user:s3cret@" + host + ":9000/v1",
	})
	if _, err := gw.Decide(ctx, []modelgateway.Candidate{{Provider: "addressed", Model: addressed, Priority: 0}}, map[string]any{}, ""); err != nil {
		t.Fatalf("Gateway.Decide: %v", err)
	}

	// The CONTROL provider does not implement ServerAddresser at all, which is the absent state:
	// "this provider has no network endpoint to name", not "we forgot".
	gw.RegisterProvider("silent", renamingFakeProvider{served: silent})
	if _, err := gw.Decide(ctx, []modelgateway.Candidate{{Provider: "silent", Model: silent, Priority: 0}}, map[string]any{}, ""); err != nil {
		t.Fatalf("Gateway.Decide (silent): %v", err)
	}

	if err := flush(ctx); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}

	// 1. The attribute is queryable by host, which is the whole point: a self-hosted deployment has
	//    many machines serving one model id under one provider name, and without this they are one
	//    line in every panel.
	waitForTempoSpan(t, tempoURL,
		fmt.Sprintf(`{ name = "chat" && span.server.address = "%s" }`, host), 30*time.Second)

	// 2. NO CREDENTIAL AND NO PORT SURVIVED. Asserted against Tempo and not only against HostOf,
	//    because what matters is what left the process, not what a helper returned.
	if n, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(
		`{ name = "chat" && span.gen_ai.request.model = "%s" && span.server.address = "%s" }`,
		addressed, host+":9000")); err != nil || n != 0 {
		t.Fatalf("a span carries host:port under server.address (matched=%d err=%v) — the port belongs "+
			"to server.port, and host:port under this name is our own shape wearing a standard one", n, err)
	}

	// 3. CONTROL: the silent span has to BE there, or the absence below proves only that nothing
	//    arrived.
	waitForTempoSpan(t, tempoURL,
		fmt.Sprintf(`{ name = "chat" && span.gen_ai.request.model = "%s" }`, silent), 30*time.Second)
	if n, err := tempoSearchCount(t, tempoURL, fmt.Sprintf(
		`{ name = "chat" && span.gen_ai.request.model = "%s" && span.server.address != nil }`, silent)); err != nil || n != 0 {
		t.Fatalf("a provider with no endpoint reports a server.address (matched=%d err=%v) — an empty or "+
			"invented host says we looked and found nothing, which is a different claim from having none", n, err)
	}
}
