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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"gopkg.in/yaml.v3"
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
        priority: 0
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

type tempoSearchResponse struct {
	Traces []struct {
		TraceID         string `json:"traceID"`
		RootServiceName string `json:"rootServiceName"`
		RootTraceName   string `json:"rootTraceName"`
		DurationMs      int    `json:"durationMs"`
	} `json:"traces"`
}

// runTrace is `aeon trace <run_id>` (DX-002): queries a real Tempo for every span this run
// produced — OBS-001's invoke_agent span carries the run_id as gen_ai.agent.name, the same
// attribute go/internal/api/tracing_integration_test.go searches by. A run with no matching
// traces (e.g. one that never went through the traced Run Controller path) is reported honestly,
// not an error — there is nothing wrong with the command itself.
func runTrace(w io.Writer, runID string) error {
	tempoURL := resolveTempoQueryURL()
	traceQL := fmt.Sprintf(`{ span.gen_ai.agent.name = %q }`, runID)
	reqURL := tempoURL + "/api/search?" + url.Values{"q": {traceQL}, "limit": {"50"}}.Encode()

	resp, err := http.Get(reqURL) //nolint:noctx // a CLI one-shot call, no surrounding context to propagate
	if err != nil {
		return fmt.Errorf("querying Tempo at %s: %w", tempoURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading Tempo response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tempo at %s returned %d: %s", tempoURL, resp.StatusCode, string(body))
	}

	var parsed tempoSearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("decoding Tempo response: %w", err)
	}

	if len(parsed.Traces) == 0 {
		fmt.Fprintf(w, "no traces found for run_id=%s\n", runID)
		return nil
	}
	for _, t := range parsed.Traces {
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

// runReplay is `aeon replay <run_id>` (DX-002): fetches a run's real Temporal workflow history —
// the actual recorded event sequence a replay would apply, not a re-execution or diff (that's
// EVAL/observability polish for later — see backlog.md). A run_id with no history is reported
// honestly, not an error.
func runReplay(w io.Writer, runID string) error {
	address := resolveTemporalAddr()
	c, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		return fmt.Errorf("connecting to Temporal at %s: %w", address, err)
	}
	defer c.Close()

	iter := c.GetWorkflowHistory(context.Background(), runID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	count := 0
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
		fmt.Fprintf(w, "%d\t%s\t%s\n", event.GetEventId(), event.GetEventTime().AsTime().Format(time.RFC3339), event.GetEventType())
	}
	if count == 0 {
		fmt.Fprintf(w, "no history events found for run_id=%s\n", runID)
	}
	return nil
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
