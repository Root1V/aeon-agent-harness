package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- init ---------------------------------------------------------------------------------

// TestAeonInit is DX-002's acceptance test for `aeon init`: the scaffolded manifests must be real
// and valid — `aeon validate` (FND-003) accepts every one of them as written, not just "some files
// got created".
func TestAeonInit(t *testing.T) {
	root := repoRoot(t)
	withSchemasDir(t, filepath.Join(root, "proto"))

	dir := filepath.Join(t.TempDir(), "my-new-agent")
	if err := runInit(dir); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	for _, name := range []string{"agent.yaml", "policy_bundle.yaml", "model_policy_bundle.yaml"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
		if err := validate(path); err != nil {
			t.Errorf("aeon validate %s = %v, want nil (a scaffolded manifest must validate as written)", name, err)
		}
	}

	t.Run("refuses to scaffold into a non-empty directory", func(t *testing.T) {
		nonEmpty := t.TempDir()
		if err := os.WriteFile(filepath.Join(nonEmpty, "existing.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runInit(nonEmpty); err == nil {
			t.Error("runInit(non-empty dir) = nil error, want one")
		}
	})
}

// ---- run ----------------------------------------------------------------------------------

// TestAeonRun mirrors TestAeonEvalRun's scope: the real engine (aeon_sdk, DX-001) can't run inside
// this Go test's own container (no Python toolchain there). This proves the CLI's own
// responsibility — finding run.py, invoking the configured interpreter with the right args, and
// reporting clearly when it can't find one — using a fake, fully controlled script.
func TestAeonRun(t *testing.T) {
	t.Run("requires an example directory", func(t *testing.T) {
		if err := runRun(io.Discard, io.Discard, "", "a query"); err == nil {
			t.Error("runRun(\"\", ...) = nil error, want one for a missing directory")
		}
	})

	t.Run("errors clearly when the example directory has no run.py", func(t *testing.T) {
		if err := runRun(io.Discard, io.Discard, t.TempDir(), ""); err == nil {
			t.Error("expected an error when run.py doesn't exist in the given directory")
		}
	})

	t.Run("reports clearly when the configured python engine can't be found", func(t *testing.T) {
		exampleDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(exampleDir, "run.py"), []byte("# unused\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AEON_EVAL_PYTHON_BIN", "aeon-eval-python-that-does-not-exist")

		err := runRun(io.Discard, io.Discard, exampleDir, "")
		if err == nil {
			t.Fatal("expected an error when the configured python binary can't be found")
		}
		if !strings.Contains(err.Error(), "make run") {
			t.Errorf("error should point the user at the make run fallback, got: %v", err)
		}
	})

	t.Run("invokes the configured python engine with the resolved script path and query", func(t *testing.T) {
		exampleDir := t.TempDir()
		scriptPath := filepath.Join(exampleDir, "run.py")
		if err := os.WriteFile(scriptPath, []byte("# unused\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fakePython := filepath.Join(t.TempDir(), "fake-python")
		if err := os.WriteFile(fakePython, []byte("#!/bin/sh\necho \"ran: $@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AEON_EVAL_PYTHON_BIN", fakePython)
		t.Setenv("AEON_PYTHON_DIR", t.TempDir())

		var stdout bytes.Buffer
		if err := runRun(&stdout, io.Discard, exampleDir, "my query"); err != nil {
			t.Fatalf("runRun: %v", err)
		}
		if !strings.Contains(stdout.String(), scriptPath) || !strings.Contains(stdout.String(), "my query") {
			t.Errorf("expected the fake engine to be invoked with the script path and query, got: %q", stdout.String())
		}
	})
}

// ---- trace --------------------------------------------------------------------------------

// TestAeonTrace proves `aeon trace` queries Tempo's real search API shape (the same one
// go/internal/api/tracing_integration_test.go exercises against a real Tempo) and handles both a
// hit and a genuine miss honestly.
func TestAeonTrace(t *testing.T) {
	t.Run("prints traces Tempo actually returns", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.URL.Query().Get("q"), "run-123") {
				t.Errorf("expected the TraceQL query to reference the run_id, got: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"traces": []map[string]any{
					{"traceID": "abc123", "rootServiceName": "aeon-runcontroller", "rootTraceName": "invoke_agent", "durationMs": 42},
				},
			})
		}))
		defer srv.Close()
		t.Setenv("AEON_TEMPO_QUERY_URL", srv.URL)

		var buf bytes.Buffer
		if err := runTrace(&buf, "run-123"); err != nil {
			t.Fatalf("runTrace: %v", err)
		}
		if !strings.Contains(buf.String(), "abc123") || !strings.Contains(buf.String(), "invoke_agent") {
			t.Errorf("expected the trace summary in output, got: %q", buf.String())
		}
	})

	t.Run("reports honestly when no traces exist for this run_id", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"traces": []map[string]any{}})
		}))
		defer srv.Close()
		t.Setenv("AEON_TEMPO_QUERY_URL", srv.URL)

		var buf bytes.Buffer
		if err := runTrace(&buf, "a-run-with-no-spans"); err != nil {
			t.Fatalf("runTrace: %v", err)
		}
		if !strings.Contains(buf.String(), "no traces found") {
			t.Errorf("expected a 'no traces found' message, got: %q", buf.String())
		}
	})

	t.Run("surfaces a non-200 Tempo response as an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("tempo is unhappy"))
		}))
		defer srv.Close()
		t.Setenv("AEON_TEMPO_QUERY_URL", srv.URL)

		if err := runTrace(io.Discard, "run-123"); err == nil {
			t.Error("expected an error for a non-200 Tempo response")
		}
	})
}

// ---- replay -------------------------------------------------------------------------------

// TestAeonReplay is a real integration test against a real Temporal server (self-skips without
// AEON_TEST_TEMPORAL_ADDRESS — see make test-go-integration). There is no Go-side workflow to
// start in this repo (every real workflow is Python, aeon_worker) — this proves the CLI's own
// responsibility, connecting to Temporal and querying history, against a genuine server.
func TestAeonReplay(t *testing.T) {
	addr := os.Getenv("AEON_TEST_TEMPORAL_ADDRESS")
	if addr == "" {
		t.Skip("AEON_TEST_TEMPORAL_ADDRESS not set — skipping Temporal integration test (see make test-go-integration)")
	}
	t.Setenv("AEON_TEMPORAL_ADDRESS", addr)

	var buf bytes.Buffer
	if err := runReplay(&buf, "not-a-real-workflow-id-xyz"); err != nil {
		t.Fatalf("runReplay: %v", err)
	}
	if !strings.Contains(buf.String(), "no history events found") {
		t.Errorf("expected a 'no history' message for a run_id that never started, got: %q", buf.String())
	}
}

// ---- publish --------------------------------------------------------------------------------

// fakeControlPlane is a minimal, real in-memory stand-in for aeon-controlplane's registry HTTP
// surface (go/internal/api/registry_handlers.go) — same routes, same status codes, same JSON
// shapes — so TestAeonPublish exercises runPublish's real HTTP client logic without needing
// Postgres.
func newFakeControlPlane(t *testing.T) *httptest.Server {
	t.Helper()
	agents := map[string]*agentRecord{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /agents", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Manifest map[string]any `json:"manifest"`
			Owner    string         `json:"owner"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		metadata, _ := body.Manifest["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		version, _ := metadata["version"].(string)
		key := name + "@" + version

		if _, exists := agents[key]; exists {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "already exists"})
			return
		}
		rec := &agentRecord{Name: name, Version: version, Owner: body.Owner, Lifecycle: "Draft", Manifest: body.Manifest}
		agents[key] = rec
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(rec)
	})
	mux.HandleFunc("GET /agents/{name}/{version}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("name") + "@" + r.PathValue("version")
		rec, ok := agents[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(rec)
	})
	mux.HandleFunc("POST /agents/{name}/{version}/transition", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("name") + "@" + r.PathValue("version")
		rec, ok := agents[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		var body struct {
			Target string `json:"target"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.Lifecycle = body.Target
		_ = json.NewEncoder(w).Encode(rec)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeTempAgentManifest(t *testing.T, name, version string) string {
	t.Helper()
	content := "apiVersion: harness.ai/v1\nkind: Agent\nmetadata:\n  name: " + name + "\n  version: " + version +
		"\n  owner: test-owner\nspec:\n  modelPolicy:\n    profile: reasoning-balanced\n  tools: {}\n  runtime: {}\n  contextPolicy: {}\n"
	return writeTempManifest(t, content)
}

// TestAeonPublish is DX-002's acceptance test for `aeon publish`: validates the manifest, registers
// it with the control plane, and promotes Draft -> Candidate — against a real (fake, but
// wire-compatible) control plane HTTP server, and idempotently re-runnable against an
// already-registered agent.
func TestAeonPublish(t *testing.T) {
	root := repoRoot(t)
	withSchemasDir(t, filepath.Join(root, "proto"))
	srv := newFakeControlPlane(t)
	t.Setenv("AEON_CONTROLPLANE_ADDR", strings.TrimPrefix(srv.URL, "http://"))

	manifestPath := writeTempAgentManifest(t, "publish-test-agent", "0.1.0")

	t.Run("registers a new agent and promotes it to Candidate", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runPublish(&buf, manifestPath); err != nil {
			t.Fatalf("runPublish: %v", err)
		}
		if !strings.Contains(buf.String(), "publish-test-agent@0.1.0") || !strings.Contains(buf.String(), "Candidate") {
			t.Errorf("expected the published agent's identity and Candidate lifecycle in output, got: %q", buf.String())
		}
	})

	t.Run("re-running publish against the same manifest picks up the existing record instead of failing", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runPublish(&buf, manifestPath); err != nil {
			t.Fatalf("runPublish (second run): %v", err)
		}
		if !strings.Contains(buf.String(), "Candidate") {
			t.Errorf("expected the already-Candidate agent reported, got: %q", buf.String())
		}
	})

	t.Run("refuses to publish an invalid manifest", func(t *testing.T) {
		badPath := writeTempManifest(t, "apiVersion: harness.ai/v1\nkind: Agent\nmetadata:\n  name: bad\n  version: 0.1.0\n")
		if err := runPublish(io.Discard, badPath); err == nil {
			t.Error("expected an error for a manifest missing 'spec'")
		}
	})
}
