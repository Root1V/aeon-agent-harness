package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

// The recording Synaptum asked for (`SYN-AEON-002`, 2026-10-25): *«no pido que documentéis una API.
// Es una grabación … POST /decide → denegado por techo de presupuesto, línea de estado + cuerpo tal
// cual, con el bloque `aeon` dentro»*.
//
// WHY A RECORDING AND NOT A DOCUMENT, in their words and they are right: it is what worked for the
// model path. And their own reason for asking once and waiting: *«escribirlo contra una forma supuesta
// es cómo se construye un adaptador que pasa sus tests y falla contra el servidor real»*. The seam has
// no contract — none of the four in the shared folder is this one — so this file is EVIDENCE and not a
// specification, and it says so where it is published.
//
// AND IT RECORDS THE DEFECT THEY FOUND, now fixed (GOV-002): the body carries `disposition` and
// `denied_by`, so a transport can tell a governance decision from a backend failure instead of raising
// ProviderError on a non-2xx. Recorded on BOTH surfaces, because a consumer that reads one and
// implements against the other is the exact failure they described.
func TestTheBudgetRefusalRecordingMatchesWhatWeServe(t *testing.T) {
	ctx := context.Background()
	s := newAPITestStore(t)

	model := "gov002-" + randSuffix(t)
	agentName := "gov002-agent-" + randSuffix(t)
	agentRef := agentName + "@0.1.0"
	if _, err := s.AgentRegistryFor("default").Create(ctx, map[string]any{
		"apiVersion": "harness.ai/v1", "kind": "Agent",
		"metadata": map[string]any{"name": agentName, "version": "0.1.0", "owner": "test", "lifecycle": "Draft"},
		// 1500 tokens: finOpsFakeProvider reports 1000+500, so the SECOND call is refused.
		"spec": map[string]any{"runtime": map[string]any{"budgets": map[string]any{"tokens": 1500}}},
	}, "test"); err != nil {
		t.Fatalf("registering the agent: %v", err)
	}
	// Built here instead of with newGovernedOpenAIServer because that helper configures a rate with no
	// per-million prices, which records a refusal showing `spent_usd: 0` — and a published recording
	// that reads "the ceiling fired and nothing had been spent" invites exactly the misreading a
	// recording exists to prevent. With real prices the body shows a real figure beside the tokens.
	pricing := finops.NewPricingTable([]finops.Rate{{
		Provider: "fake-compute", Model: model, CostModel: "token_based",
		InputPerMillionUSD: 10, OutputPerMillionUSD: 30,
	}})
	gw := modelgateway.New()
	gw.RegisterProvider("fake-compute", &finOpsFakeProvider{costModel: "token_based"})
	governance := &ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: s, Agents: s}
	mux := http.NewServeMux()
	governance.Register(mux)
	(&OpenAICompatibleHandlers{Gateway: gw, Bundle: governedTestBundle(model), Governance: governance}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)
	runID := "gov002-run-" + randSuffix(t)

	type exchange struct {
		Why      string            `json:"why"`
		Method   string            `json:"method"`
		Path     string            `json:"path"`
		Headers  map[string]string `json:"headers"`
		Request  json.RawMessage   `json:"request"`
		Status   int               `json:"status"`
		Response json.RawMessage   `json:"response"`
	}
	var exchanges []exchange

	headers := map[string]string{RunIDHeader: runID, AgentManifestRefHeader: agentRef}
	chat := map[string]any{"model": "governed-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	// First call: allowed, and it spends the ceiling.
	status, body := postChatCompletionWithHeaders(t, srv, chat, headers)
	if status != http.StatusOK {
		t.Fatalf("the first call was refused before the ceiling: %d %v", status, body)
	}

	// Second call on the OpenAI-compatible surface: refused.
	status, body = postChatCompletionWithHeaders(t, srv, chat, headers)
	if status != http.StatusPaymentRequired {
		t.Fatalf("second call on /v1/chat/completions = %d, want 402: %v", status, body)
	}
	raw, _ := json.Marshal(body)
	req, _ := json.Marshal(chat)
	exchanges = append(exchanges, exchange{
		Why: "el techo ya está gastado: la segunda llamada se rechaza. OpenAI-compatible, así que el " +
			"motivo viaja en el sobre de error de OpenAI y los hechos en el bloque `aeon`",
		Method: "POST", Path: "/v1/chat/completions", Headers: headers,
		Request: req, Status: status, Response: raw,
	})

	// And the same refusal on /decide, which is the surface they asked about.
	decideBody := decideRequest{
		Candidates:      []decideCandidate{{Provider: "fake-compute", Model: model, Priority: 0}},
		RenderedContext: map[string]any{"messages": []any{}},
		RunID:           runID, AgentManifestRef: agentRef,
	}
	status, body = postDecide(t, srv, decideBody)
	if status != http.StatusPaymentRequired {
		t.Fatalf("POST /decide = %d, want 402: %v", status, body)
	}
	raw, _ = json.Marshal(body)
	req, _ = json.Marshal(decideBody)
	exchanges = append(exchanges, exchange{
		Why: "el mismo rechazo por /decide, que es la superficie por la que preguntaron. Aquí el cuerpo " +
			"NO lleva sobre de OpenAI: el rechazo es la respuesta entera",
		Method: "POST", Path: "/decide", Headers: nil, Request: req, Status: status, Response: raw,
	})

	// The two assertions that make this a recording of a GOVERNANCE decision and not of an error.
	for _, e := range exchanges {
		var parsed map[string]any
		if err := json.Unmarshal(e.Response, &parsed); err != nil {
			t.Fatalf("decoding %s %s: %v", e.Method, e.Path, err)
		}
		facts := parsed
		if nested, ok := parsed["aeon"].(map[string]any); ok {
			facts = nested
		}
		if facts["disposition"] != "terminate_run" {
			t.Errorf("%s %s: disposition = %v, want terminate_run — without it their transport sees a "+
				"non-2xx and raises ProviderError, so the run stops as a backend failure and nothing can "+
				"tell the three dispositions apart", e.Method, e.Path, facts["disposition"])
		}
		if facts["denied_by"] != "budget" {
			t.Errorf("%s %s: denied_by = %v, want budget — the same value the span carries (OBS-020), so "+
				"a trace and a response cannot disagree about who stopped the run", e.Method, e.Path, facts["denied_by"])
		}
	}

	recording := map[string]any{
		"$comment": "Grabación de un rechazo por techo de presupuesto, producida y verificada por " +
			"TestTheBudgetRefusalRecordingMatchesWhatWeServe contra Postgres real y el registro de " +
			"agentes real. Pedida por Synaptum en SYN-AEON-002.",
		"$no_es_un_contrato": "ESTA COSTURA NO TIENE CONTRATO. Ninguno de los cuatro de la carpeta " +
			"compartida es el de gobierno, y esto no lo inventa: es evidencia de la forma que servimos " +
			"hoy, para que un cliente no se escriba contra una forma supuesta. Si de aquí sale un " +
			"contrato, se acuerda aparte.",
		"$volatiles": "run_id, el ref del agente y el modelo se sustituyen por marcadores. Los números " +
			"del techo y del gasto NO: son la afirmación.",
		"exchanges": exchanges,
	}
	got, err := json.MarshalIndent(recording, "", "  ")
	if err != nil {
		t.Fatalf("encoding the recording: %v", err)
	}
	got = regexp.MustCompile(regexp.QuoteMeta(runID)).ReplaceAll(got, []byte("{run_id}"))
	got = regexp.MustCompile(regexp.QuoteMeta(agentRef)).ReplaceAll(got, []byte("{agent}@0.1.0"))
	got = regexp.MustCompile(regexp.QuoteMeta(model)).ReplaceAll(got, []byte("{model}"))

	path := filepath.Join("..", "..", "..", "evals", "recordings", "gobierno",
		"rechazo-por-techo.json")
	if os.Getenv("AEON_RECORD") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating the recording directory: %v", err)
		}
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatalf("writing the recording: %v", err)
		}
		t.Logf("recording rewritten: %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the checked-in recording (AEON_RECORD=1 to create it): %v", err)
	}
	if string(want) != string(got)+"\n" {
		t.Errorf("the refusal no longer looks like the published recording.\n\nserved:\n%s\n\nrecorded:\n%s\n\n"+
			"If the change is intended, rerun with AEON_RECORD=1 and say so in the channel — Synaptum is "+
			"writing a client against this file.", got, want)
	}
}
