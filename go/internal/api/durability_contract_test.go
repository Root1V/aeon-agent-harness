package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
)

// TestTheDurabilitySeamMatchesTheSharedContract validates what this service actually puts on the
// wire against the JSON Schemas in evals/contracts/costura-durabilidad — the durability seam agreed
// with Synaptum.
//
// WHY IT DID NOT EXIST AND HAD TO, found on 2026-10-08 while adding `sub_run_id` for VRT-AEON-004:
// those three schemas were checked by NOTHING. `make lint` parses `proto/schemas/*.json` and
// `proto/manifests/*.json`; the contract lives under `evals/`, so it was not even syntax-checked,
// and no test validated a single request or response against it. So the append request's
// `additionalProperties: false` — whose own description explains that an extra field there "es una
// errata en la mitad de una clave de idempotencia" — was a claim nobody enforced, in a document
// another team implements against. Our wire shape could have drifted from it at any point with
// everything green.
//
// It is also what makes bumping the contract to 0.2 mean something: adding `sub_run_id` to our
// request struct without adding it to the schema would have made every request we send INVALID
// against the contract, and this test is the only thing that would have said so.
func TestTheDurabilitySeamMatchesTheSharedContract(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "evals", "contracts", "costura-durabilidad", "schema")
	compile := func(t *testing.T, name string) *jsonschema.Schema {
		t.Helper()
		path := filepath.Join(dir, name)
		raw, err := os.Open(path)
		if err != nil {
			// NOT a skip: the contract is in this repository, so an unreadable file means it moved or
			// was deleted and the comparison this test exists to make has silently stopped happening.
			t.Fatalf("opening %s: %v", path, err)
		}
		defer raw.Close()
		doc, err := jsonschema.UnmarshalJSON(raw)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(name, doc); err != nil {
			t.Fatalf("adding %s: %v", name, err)
		}
		schema, err := c.Compile(name)
		if err != nil {
			t.Fatalf("compiling %s: %v", name, err)
		}
		return schema
	}

	requestSchema := compile(t, "append-request.schema.json")
	resultSchema := compile(t, "append-result.schema.json")
	stateSchema := compile(t, "run-state.schema.json")

	// Through the real types, encoded the way the wire sees them, rather than against a hand-written
	// JSON literal — a literal would only prove that the literal matches the schema.
	validate := func(t *testing.T, schema *jsonschema.Schema, label string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("encoding %s: %v", label, err)
		}
		decoded, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("re-decoding %s: %v", label, err)
		}
		if err := schema.Validate(decoded); err != nil {
			t.Errorf("%s does not satisfy the shared contract:\n%s\n%v", label, raw, err)
		}
	}

	t.Run("an append request with a sub-run is valid", func(t *testing.T) {
		// The case that would have failed before the schema was bumped: additionalProperties is false,
		// so an unknown `sub_run_id` is a hard rejection rather than a tolerated extra.
		validate(t, requestSchema, "append request with a sub-run", appendCheckpointRequest{
			SubRunID: "child-b/grandchild",
			StepID:   "s1",
			Phase:    string(checkpoint.PhaseCompleted),
			Payload:  json.RawMessage(`{"ok":true}`),
		})
	})

	t.Run("an append request without one is still valid", func(t *testing.T) {
		// 0.1 compatibility, asserted rather than assumed: a client that knows nothing about
		// delegation omits the field, and `omitempty` has to actually omit it.
		validate(t, requestSchema, "append request with no sub-run", appendCheckpointRequest{
			StepID: "s1", Phase: string(checkpoint.PhaseAttempted),
		})
	})

	t.Run("the real responses are valid", func(t *testing.T) {
		// Through the real handler and a real store, so these are the bytes a consumer receives.
		s := newAPITestStore(t)
		mux := http.NewServeMux()
		(&CheckpointHandlers{Store: s}).Register(mux)
		srv := httptest.NewServer(authWrap(t, mux))
		t.Cleanup(srv.Close)

		runID := "contract-" + randSuffix(t)
		body, _ := json.Marshal(appendCheckpointRequest{
			SubRunID: "child-a", StepID: "s1", Phase: string(checkpoint.PhaseCompleted),
			Payload: json.RawMessage(`{"ok":true}`),
		})
		resp := postJSONAuthed(t, srv.URL+"/runs/"+runID+"/checkpoints", body)
		var result map[string]any
		func() {
			defer resp.Body.Close()
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatalf("decoding the append result: %v", err)
			}
		}()
		validate(t, resultSchema, "append result", result)

		got := getAuthed(t, srv.URL+"/runs/"+runID+"/checkpoints")
		var state map[string]any
		func() {
			defer got.Body.Close()
			if err := json.NewDecoder(got.Body).Decode(&state); err != nil {
				t.Fatalf("decoding the run state: %v", err)
			}
		}()
		validate(t, stateSchema, "run state", state)
		// And the field really is on the wire, not merely permitted by a schema that tolerates extras:
		// run-state's records set additionalProperties: true on purpose, so validation alone would
		// pass with sub_run_id missing entirely.
		records, _ := state["records"].([]any)
		if len(records) != 1 {
			t.Fatalf("the journal returned %d records, want 1", len(records))
		}
		first, _ := records[0].(map[string]any)
		if first["sub_run_id"] != "child-a" {
			t.Errorf("the returned record's sub_run_id is %v, want child-a — the field is dropped on "+
				"the way out and the schema's additionalProperties: true would never say so",
				first["sub_run_id"])
		}
	})
}
