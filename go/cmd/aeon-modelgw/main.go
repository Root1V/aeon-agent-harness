// Command aeon-modelgw is the Model Gateway (MDL-001): the only component in the platform allowed
// to talk to a model provider directly. It resolves a capability profile to a concrete
// provider/model pair via a ModelPolicyBundle, tries candidates in priority order with fallback,
// and exposes that over HTTP (POST /decide, go/internal/api/model_gateway_handlers.go) — the
// Python worker's model.decide Activity (DR-001 onward) is this endpoint's first real caller. See
// docs/adr/0004-model-gateway-provider-abstraction.md.
//
// Each of the 5 adapters (go/internal/providers/*) is registered only when its required
// configuration is present in the environment (see .env.example) — a candidate naming an
// unconfigured provider fails over to the next candidate (modelgateway.Gateway's normal fallback),
// rather than this process refusing to start.
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/aeon-ai/aeon/go/internal/api"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/providers/anthropic"
	"github.com/aeon-ai/aeon/go/internal/providers/gemini"
	"github.com/aeon-ai/aeon/go/internal/providers/openai"
	openaicompatible "github.com/aeon-ai/aeon/go/internal/providers/openai_compatible"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
	"github.com/aeon-ai/aeon/go/internal/tracing"
)

func main() {
	if p := os.Getenv("AEON_MODELGW_PORT"); p != "" {
		os.Setenv("AEON_PORT", p)
	} else {
		os.Setenv("AEON_PORT", "9402")
	}

	otelEndpoint := os.Getenv("AEON_OTEL_ENDPOINT")
	if otelEndpoint == "" {
		otelEndpoint = "otel-collector:4318"
	}
	if _, shutdown, err := tracing.Init(context.Background(), "aeon-modelgw", otelEndpoint); err != nil {
		log.Printf("aeon-modelgw: tracing disabled: %v", err)
	} else {
		defer shutdown(context.Background())
	}

	gw := modelgateway.New()
	registered := registerProvidersFromEnv(gw)
	log.Printf("aeon-modelgw: registered providers: %v", registered)

	mux := http.NewServeMux()
	(&api.ModelGatewayHandlers{Gateway: gw}).Register(mux)

	srv := httpserver.New("aeon-modelgw", mux)
	log.Println("aeon-modelgw starting")
	httpserver.MustListenAndServe(srv)
}

// registerProvidersFromEnv wires each adapter whose required credentials/endpoints are present in
// the environment (see .env.example) and returns the names actually registered.
func registerProvidersFromEnv(gw *modelgateway.Gateway) []string {
	var registered []string

	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		gw.RegisterProvider(anthropic.Name, &anthropic.Adapter{APIKey: key})
		registered = append(registered, anthropic.Name)
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		gw.RegisterProvider(openai.Name, &openai.Adapter{APIKey: key})
		registered = append(registered, openai.Name)
	}
	if key := os.Getenv("GOOGLE_API_KEY"); key != "" {
		gw.RegisterProvider(gemini.Name, &gemini.Adapter{APIKey: key})
		registered = append(registered, gemini.Name)
	}
	if clientID := os.Getenv("PROMETHEUS_CLIENT_ID"); clientID != "" {
		gw.RegisterProvider(prometheusinference.Name, prometheusinference.New(
			os.Getenv("PROMETHEUS_AUTH_URL"),
			os.Getenv("PROMETHEUS_GATEWAY_URL"),
			clientID,
			os.Getenv("PROMETHEUS_CLIENT_SECRET"),
			os.Getenv("PROMETHEUS_SCOPE"),
			os.Getenv("PROMETHEUS_DEFAULT_MODEL"),
		))
		registered = append(registered, prometheusinference.Name)
	}
	if baseURL := os.Getenv("OPENAI_COMPATIBLE_BASE_URL"); baseURL != "" {
		gw.RegisterProvider(openaicompatible.Name, &openaicompatible.Adapter{
			BaseURL: baseURL,
			APIKey:  os.Getenv("OPENAI_COMPATIBLE_API_KEY"),
		})
		registered = append(registered, openaicompatible.Name)
	}

	return registered
}
