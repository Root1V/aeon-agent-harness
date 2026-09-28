// DX-002: the remaining CLI subcommands beyond validate/eval (already in main.go) — init, run,
// trace, replay, publish. Split into its own file for readability; still package main.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"gopkg.in/yaml.v3"

	"github.com/aeon-ai/aeon/go/internal/tempoclient"
)

// ---- init -------------------------------------------------------------------------------------

const initAgentManifestTemplate = `apiVersion: harness.ai/v1
kind: Agent
metadata:
  name: my-agent
  version: 0.1.0
  owner: your-team
spec:
  modelPolicy:
    profile: reasoning-balanced
  tools:
    allow: [search.web]
  runtime:
    maxTurns: 24
  contextPolicy: {}
`

const initPolicyBundleTemplate = `apiVersion: harness.ai/v1
kind: PolicyBundle
cedarVersion: "4.0"
policies:
  - id: allow-my-agent-tools
    effect: permit
    cedarSource: |
      permit(
        principal == Agent::"my-agent@0.1.0",
        action,
        resource
      ) when {
        ["search.web"].contains(resource.name)
      };
`

const initModelPolicyBundleTemplate = `apiVersion: harness.ai/v1
kind: ModelPolicyBundle
profiles:
  - profile: reasoning-balanced
    candidates:
      - provider: openai_compatible
        model: local-default
        modality: text
        inference_class: local
        priority: 0
        # OBS-009: cost_model is required, and this scaffold is where that requirement earns its
        # keep. A candidate without it is skipped by the pricing table, so every call to this model
        # would land in the FinOps ledger with no cost — silently, because nobody wrote a field.
        # A local dev server bills nothing, so compute_based with no rates is the honest answer
        # here; what the schema forbids is arriving at "unpriced" by omission.
        cost_model: compute_based
    # MDL-008: local inference resolves in Prometheus, and nowhere else, unless an exception names
    # the provider and the environment it belongs to. This scaffold runs against Ollama/vLLM on a
    # developer machine, which is exactly the case the exception exists for — and writing it down
    # here is what keeps "why is this allowed" answerable without asking anyone.
    local_inference:
      environment: local-dev
      allowed_providers: [openai_compatible]
`

// runInit scaffolds a new agent project directory with minimal, valid config-as-code manifests
// (FND-003) — real templates that pass `aeon validate` as written, not just placeholders.
func runInit(dir string) error {
	if dir == "" {
		return fmt.Errorf("directory is required")
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s already exists and is not empty", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	files := map[string]string{
		"agent.yaml":               initAgentManifestTemplate,
		"policy_bundle.yaml":       initPolicyBundleTemplate,
		"model_policy_bundle.yaml": initModelPolicyBundleTemplate,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	return nil
}

// ---- run ----------------------------------------------------------------------------------

// runRun is `aeon run <example-directory> [query]` (DX-002): looks for <example-directory>/run.py
// (the aeon_sdk-based entrypoint DX-001 established — see examples/deep-research/run.py) and
// shells out to it, same interpreter-resolution pattern as `aeon eval run` (EVAL-002): the real
// engine lives in Python, and shouldn't get a second, duplicate implementation here.
func runRun(stdout, stderr io.Writer, exampleDir, query string) error {
	if exampleDir == "" {
		return fmt.Errorf("example directory is required")
	}
	scriptPath, err := filepath.Abs(filepath.Join(exampleDir, "run.py"))
	if err != nil {
		return err
	}
	if _, err := os.Stat(scriptPath); err != nil {
		return fmt.Errorf("no run.py found in %s (expected %s): %w", exampleDir, scriptPath, err)
	}

	bin := pythonBin()
	binPath, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf(
			"%s not found on PATH — this example's engine lives in Python (aeon_sdk); "+
				"run it via the Python container instead: `make run EXAMPLE=%s`", bin, exampleDir,
		)
	}

	pythonDir, err := resolvePythonDir()
	if err != nil {
		return err
	}

	args := []string{scriptPath}
	if query != "" {
		args = append(args, query)
	}
	cmd := exec.Command(binPath, args...)
	cmd.Dir = pythonDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run exited with an error: %w", err)
	}
	return nil
}

// ---- trace --------------------------------------------------------------------------------

// resolveTempoQueryURL mirrors resolveProtoDir/resolveEvalsDir's env-var-first pattern —
// AEON_TEMPO_QUERY_URL, defaulting to the compose stack's own service address.
func resolveTempoQueryURL() string {
	if url := os.Getenv("AEON_TEMPO_QUERY_URL"); url != "" {
		return url
	}
	return "http://localhost:3200"
}

// runTrace is `aeon trace <run_id>` (DX-002): queries a real Tempo for every span this run
// produced — OBS-001's invoke_agent span carries the run_id as gen_ai.agent.name, the same
// attribute go/internal/api/tracing_integration_test.go searches by. A run with no matching
// traces (e.g. one that never went through the traced Run Controller path) is reported honestly,
// not an error — there is nothing wrong with the command itself. Shares its Tempo query with the
// Agent Console (OBS-002, go/internal/tempoclient) rather than re-implementing it.
func runTrace(w io.Writer, runID string) error {
	traces, err := tempoclient.SearchByAgentName(context.Background(), resolveTempoQueryURL(), runID)
	if err != nil {
		return err
	}
	if len(traces) == 0 {
		fmt.Fprintf(w, "no traces found for run_id=%s\n", runID)
		return nil
	}
	for _, t := range traces {
		fmt.Fprintf(w, "%s\t%s\t%s\t%dms\n", t.TraceID, t.RootServiceName, t.RootTraceName, t.DurationMs)
	}
	return nil
}

// ---- replay -------------------------------------------------------------------------------

// resolveTemporalAddr mirrors the AEON_TEMPORAL_ADDRESS convention already used by
// aeon-runcontroller/aeon-worker — the same env var, the same default.
func resolveTemporalAddr() string {
	if addr := os.Getenv("AEON_TEMPORAL_ADDRESS"); addr != "" {
		return addr
	}
	return "localhost:7233"
}

// replayCommandFor is the command that actually replays a run, printed by --assert-identical.
//
// It names the WORKER image because that is where the workflow definitions live. This CLI is a static Go
// binary: a Go replayer has no AgentRunWorkflow to replay a history against, so it cannot do this itself,
// and pretending to would be the worst of the three options. See python/aeon_worker/replay.py.
func replayCommandFor(runID string) string {
	// `uv run` and not a bare `python`: the worker image installs its dependencies into a uv-managed
	// virtualenv (deploy/compose/Dockerfile.python), so a bare `python -m` cannot import temporalio. The
	// first version of this string omitted it and failed with ModuleNotFoundError — found by running the
	// command this function prints, which is the only way to find out that a printed command is wrong.
	return "docker compose -f deploy/compose/docker-compose.yml exec worker " +
		"uv run python -m aeon_worker.replay " + runID
}

// runReplay is `aeon replay <run_id>` (DX-002): prints a run's real Temporal workflow history — the
// actual recorded event sequence a replay would apply.
//
// A run_id with no history is reported honestly, not as an error.
func runReplay(w io.Writer, runID string) error {
	return replayRun(w, runID, false)
}

// runReplayAssertIdentical is `aeon replay <run_id> --assert-identical`.
//
// WHAT IT CAN ANSWER FROM HERE, and it is worth having: whether this run ALREADY suffered a
// non-determinism failure. Temporal records that as a WorkflowTaskFailed event with cause
// NON_DETERMINISTIC_ERROR, so it is a fact about the recorded history and needs no replay at all — and it
// is the one question a person asking "did determinism hold?" most often actually means, because a run
// that already broke is a run that broke in production.
//
// WHAT IT CANNOT: a fresh replay against today's code. That needs the workflow definitions, which are
// Python. So it exits non-zero on a recorded failure, and otherwise says plainly that the history holds no
// failure AND that a fresh replay is a different check, with the command that performs it. Reporting
// "identical" from here would be claiming a verification that never ran.
func runReplayAssertIdentical(w io.Writer, runID string) error {
	return replayRun(w, runID, true)
}

func replayRun(w io.Writer, runID string, assertIdentical bool) error {
	address := resolveTemporalAddr()
	c, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		return fmt.Errorf("connecting to Temporal at %s: %w", address, err)
	}
	defer c.Close()

	iter := c.GetWorkflowHistory(context.Background(), runID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	count := 0
	var recordedFailures []string
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			// A run_id that never started any workflow isn't a real replay failure — Temporal's
			// server reports it as NotFound (surfaced with a raw "sql: no rows in result set"
			// message from its own persistence layer, not a clean not-found message), so this
			// must be treated the same as "iterated and found zero events", not propagated as an
			// error.
			var notFound *serviceerror.NotFound
			if errors.As(err, &notFound) {
				break
			}
			return fmt.Errorf("reading workflow history for %s: %w", runID, err)
		}
		count++
		if cause := nonDeterminismCause(event); cause != "" {
			recordedFailures = append(recordedFailures, fmt.Sprintf("event %d: %s", event.GetEventId(), cause))
		}
		if !assertIdentical {
			fmt.Fprintf(w, "%d\t%s\t%s\n", event.GetEventId(), event.GetEventTime().AsTime().Format(time.RFC3339), event.GetEventType())
		}
	}
	if count == 0 {
		// An absent history is NOT a passing assertion. A run_id nobody ever started and a run that
		// replays cleanly are different facts, and answering "identical" for the first would report a
		// check that never ran — the failure mode this whole command exists to avoid.
		fmt.Fprintf(w, "no history events found for run_id=%s\n", runID)
		if assertIdentical {
			return fmt.Errorf("nothing to verify for run_id=%s: an absent history is not a passing assertion", runID)
		}
		return nil
	}
	if !assertIdentical {
		return nil
	}

	if len(recordedFailures) > 0 {
		for _, f := range recordedFailures {
			fmt.Fprintf(w, "NON-DETERMINISM RECORDED IN HISTORY\t%s\n", f)
		}
		return fmt.Errorf("run %s already failed a workflow task with a non-determinism cause (%d occurrence(s)) — "+
			"the boundary of ADR-001 was crossed by the code that ran, and no fresh replay is needed to know it",
			runID, len(recordedFailures))
	}

	fmt.Fprintf(w, "run %s: %d events, and the history records NO non-determinism failure.\n", runID, count)
	fmt.Fprintf(w, "That is what a history can answer. A FRESH replay against today's code is a different "+
		"check and runs where the workflow definitions live:\n  %s\n", replayCommandFor(runID))
	return nil
}

// nonDeterminismCause returns a description when this event is a workflow task that failed on
// non-determinism, or "" otherwise.
//
// Only that cause is singled out. A workflow task can fail for many reasons — a bad Activity result, an
// unhandled error — and reporting those here would make this command answer a question it was not asked,
// which is how a signal becomes noise and stops being read.
func nonDeterminismCause(event *historypb.HistoryEvent) string {
	attrs := event.GetWorkflowTaskFailedEventAttributes()
	if attrs == nil {
		return ""
	}
	if attrs.GetCause() != enumspb.WORKFLOW_TASK_FAILED_CAUSE_NON_DETERMINISTIC_ERROR {
		return ""
	}
	if f := attrs.GetFailure(); f != nil {
		return "NON_DETERMINISTIC_ERROR: " + f.GetMessage()
	}
	return "NON_DETERMINISTIC_ERROR"
}

// ---- publish --------------------------------------------------------------------------------

// resolveControlplaneAddr mirrors the same env-var-first pattern as the rest of this file.
func resolveControlplaneAddr() string {
	if addr := os.Getenv("AEON_CONTROLPLANE_ADDR"); addr != "" {
		return addr
	}
	return "localhost:9401"
}

// runPublish is `aeon publish <manifest>` (DX-002): validates the manifest (FND-003), registers it
// with the real control plane (FND-001), and promotes a freshly-created Draft to Candidate. It
// does NOT attempt Candidate -> Released here — that step requires a ReleaseGateDecision
// (EVAL-003), which needs a baseline eval comparison this command doesn't have inputs for yet; see
// backlog.md. Re-running publish against an already-registered agent is safe: it picks up the
// existing record instead of failing on ErrAlreadyExists.
func runPublish(w io.Writer, manifestPath string) error {
	if err := validate(manifestPath); err != nil {
		return fmt.Errorf("manifest invalid, refusing to publish: %w", err)
	}

	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var parsed any
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}
	manifest, ok := parsed.(map[string]any)
	if !ok {
		return fmt.Errorf("manifest root must be a mapping/object")
	}
	if kind, _ := manifest["kind"].(string); kind != "Agent" {
		return fmt.Errorf("aeon publish only supports kind: Agent manifests today, got %q", kind)
	}

	metadata, _ := manifest["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	version, _ := metadata["version"].(string)
	owner, _ := metadata["owner"].(string)
	if owner == "" {
		owner = "unknown"
	}

	cp := &controlplaneClient{baseURL: "http://" + resolveControlplaneAddr()}

	rec, err := cp.createAgent(manifest, owner)
	if err != nil {
		if !errors.Is(err, errAlreadyRegistered) {
			return fmt.Errorf("registering agent with the control plane: %w", err)
		}
		rec, err = cp.getAgent(name, version)
		if err != nil {
			return fmt.Errorf("fetching already-registered agent %s@%s: %w", name, version, err)
		}
	}

	if rec.Lifecycle == "Draft" {
		rec, err = cp.transitionLifecycle(name, version, "Candidate", nil)
		if err != nil {
			return fmt.Errorf("promoting %s@%s Draft -> Candidate: %w", name, version, err)
		}
	}

	fmt.Fprintf(w, "agent=%s@%s owner=%s lifecycle=%s\n", rec.Name, rec.Version, rec.Owner, rec.Lifecycle)

	abomPath, fingerprint, ephemeral, err := writeABOM(manifestPath, manifest, raw)
	if err != nil {
		return fmt.Errorf("generating ABOM: %w", err)
	}
	fmt.Fprintf(w, "abom=%s public_key=%s\n", abomPath, fingerprint)
	if ephemeral {
		fmt.Fprintln(w, "warning: AEON_ABOM_SIGNING_KEY is not set — signed with a freshly generated, ephemeral key; this ABOM's signature will NOT reproduce on a future run")
	}
	return nil
}

// agentRecord mirrors go/internal/store.AgentRecord's JSON shape — kept as a local copy rather
// than importing the store package, since this CLI talks to the control plane over HTTP, never
// the store directly (the same boundary every other Aeon service respects).
type agentRecord struct {
	Name      string         `json:"name"`
	Version   string         `json:"version"`
	Owner     string         `json:"owner"`
	Lifecycle string         `json:"lifecycle"`
	Manifest  map[string]any `json:"manifest"`
}

var errAlreadyRegistered = fmt.Errorf("already registered")

type controlplaneClient struct {
	baseURL    string
	httpClient *http.Client
}

func (c *controlplaneClient) client() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return http.DefaultClient
}

func (c *controlplaneClient) createAgent(manifest map[string]any, owner string) (*agentRecord, error) {
	body, err := json.Marshal(map[string]any{"manifest": manifest, "owner": owner})
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Post(c.baseURL+"/agents", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return nil, errAlreadyRegistered
	}
	return decodeAgentRecord(resp)
}

func (c *controlplaneClient) getAgent(name, version string) (*agentRecord, error) {
	resp, err := c.client().Get(c.baseURL + "/agents/" + name + "/" + version)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeAgentRecord(resp)
}

func (c *controlplaneClient) transitionLifecycle(name, version, target string, gate map[string]any) (*agentRecord, error) {
	payload := map[string]any{"target": target}
	if gate != nil {
		payload["release_gate"] = gate
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Post(c.baseURL+"/agents/"+name+"/"+version+"/transition", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeAgentRecord(resp)
}

func decodeAgentRecord(resp *http.Response) (*agentRecord, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &errBody)
		if errBody.Error != "" {
			return nil, fmt.Errorf("control plane returned %d: %s", resp.StatusCode, errBody.Error)
		}
		return nil, fmt.Errorf("control plane returned %d: %s", resp.StatusCode, string(body))
	}
	var rec agentRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("decoding control plane response: %w", err)
	}
	return &rec, nil
}
