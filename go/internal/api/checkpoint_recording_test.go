package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
)

// The recording Synaptum asked for (`SYN`, 2026-10-24, point 4): *«si alguna vez os sobra una
// grabación de `/runs/.../checkpoints` —padre y sub-run—, es el corpus que falta»*.
//
// THEIR ARGUMENT FOR WHY IT IS MISSING, which is the reason this file exists and not a nicety: their
// test double for the durability seam DOES filter by `sub_run_id`, so their suite was green against a
// double that implemented the contract while our server did not (`INT-009c`). Nothing compared the two.
// For the model path we have recordings; for this one there were two implementations of one contract
// and no joint evidence.
//
// A GOLDEN RECORDING WITH A REAL PRODUCER, not a file someone wrote by hand. The exchanges below are
// captured from the real route against real Postgres, and then compared to the checked-in file — so
// the recording cannot go stale without this test saying so, and what it shows is what our server
// does today rather than what a document claims. Updating it is deliberate: AEON_RECORD=1 rewrites it.
func TestTheCheckpointRouteRecordingMatchesWhatWeServe(t *testing.T) {
	s := newAPITestStore(t)
	mux := http.NewServeMux()
	(&CheckpointHandlers{Store: s}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)

	runID := "recording-" + randSuffix(t)
	type exchange struct {
		Why      string          `json:"why"`
		Method   string          `json:"method"`
		Path     string          `json:"path"`
		Request  json.RawMessage `json:"request,omitempty"`
		Status   int             `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	var exchanges []exchange

	post := func(why string, body map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		resp := postJSONAuthed(t, srv.URL+"/runs/"+runID+"/checkpoints", raw)
		defer resp.Body.Close()
		var parsed json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			t.Fatalf("decoding the append result: %v", err)
		}
		exchanges = append(exchanges, exchange{
			Why: why, Method: "POST", Path: "/runs/{run_id}/checkpoints",
			Request: raw, Status: resp.StatusCode, Response: parsed,
		})
	}
	get := func(why, query string) {
		t.Helper()
		url := srv.URL + "/runs/" + runID + "/checkpoints" + query
		resp := getAuthed(t, url)
		defer resp.Body.Close()
		var parsed json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			t.Fatalf("decoding the run state: %v", err)
		}
		exchanges = append(exchanges, exchange{
			Why: why, Method: "GET", Path: "/runs/{run_id}/checkpoints" + query,
			Status: resp.StatusCode, Response: parsed,
		})
	}

	// The root run writes a step, and a sub-run writes a step WITH THE SAME id — which is the whole
	// point of the identity and the case a 0.1 implementation cannot express.
	post("el run raíz escribe un paso, sin mandar sub_run_id en absoluto",
		map[string]any{"step_id": "s1", "phase": string(checkpoint.PhaseCompleted), "payload": map[string]any{"total": 120}})
	post("un sub-run escribe un paso CON EL MISMO step_id: no es un duplicado",
		map[string]any{"sub_run_id": "child-a", "step_id": "s1", "phase": string(checkpoint.PhaseCompleted), "payload": map[string]any{"total": 340}})
	post("y el mismo paso del mismo sub-run otra vez SÍ es un duplicado",
		map[string]any{"sub_run_id": "child-a", "step_id": "s1", "phase": string(checkpoint.PhaseCompleted), "payload": map[string]any{"total": 340}})
	get("la lectura sin parámetro devuelve SOLO la raíz, y next_seq es del run entero", "")
	get("la lectura del sub-run devuelve SOLO lo suyo, con el mismo next_seq", "?sub_run_id=child-a")
	get("vacío explícito es la raíz, igual que omitido", "?sub_run_id=")

	recording := map[string]any{
		"$comment": "Grabación del camino real de GET/POST /runs/{run_id}/checkpoints contra Postgres " +
			"real, producida y verificada por TestTheCheckpointRouteRecordingMatchesWhatWeServe. Pedida " +
			"por Synaptum el 2026-10-24: su doble de test filtraba por sub_run_id y nuestro servidor no, " +
			"así que su suite estaba verde midiendo el doble. Esto es la evidencia conjunta que faltaba.",
		"$volatiles": "run_id, los ids opacos y recorded_at se sustituyen por marcadores: lo que el " +
			"corpus fija es la FORMA y los valores que son la afirmación (sub_run_id, seq, next_seq, " +
			"duplicate), no el reloj ni un uuid.",
		"contract":  "costura-durabilidad",
		"version":   "0.2-draft",
		"exchanges": exchanges,
	}
	got, err := json.MarshalIndent(recording, "", "  ")
	if err != nil {
		t.Fatalf("encoding the recording: %v", err)
	}
	got = normalizeRecording(got, runID)

	path := filepath.Join("..", "..", "..", "evals", "contracts", "costura-durabilidad",
		"fixtures", "grabacion-sub-run.json")
	if os.Getenv("AEON_RECORD") == "1" {
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatalf("writing the recording: %v", err)
		}
		t.Logf("recording rewritten: %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the checked-in recording (run with AEON_RECORD=1 to create it): %v", err)
	}
	if string(want) != string(got)+"\n" {
		t.Errorf("the route no longer serves what the published recording shows.\n\nserved:\n%s\n\n"+
			"recorded:\n%s\n\nIf the change is intended, rerun with AEON_RECORD=1 and publish the new "+
			"version and hash in the channel — other teams implement against this file.", got, want)
	}
}

// normalizeRecording replaces the values that change on every run. Narrow on purpose: a normalizer
// that touched `seq`, `next_seq`, `duplicate` or `sub_run_id` would erase the assertions.
func normalizeRecording(raw []byte, runID string) []byte {
	out := regexp.MustCompile(regexp.QuoteMeta(runID)).ReplaceAll(raw, []byte("{run_id}"))
	out = regexp.MustCompile(`"recorded_at": "[^"]+"`).ReplaceAll(out, []byte(`"recorded_at": "{timestamp}"`))
	return out
}
