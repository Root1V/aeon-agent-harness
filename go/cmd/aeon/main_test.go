package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// repoRoot resolves the repository root relative to this test file (go/cmd/aeon/main_test.go is
// two levels below go/, three below the repo root), matching the pattern already used in
// go/internal/api/tool_gateway_handlers_test.go so tests exercise the real checked-in files.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

func withSchemasDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("AEON_SCHEMAS_DIR", dir)
}

// TestAeonValidateAcceptsAndRejectsExampleManifests is FND-003's acceptance test: `aeon validate`
// must accept every real manifest under examples/deep-research (the config-as-code the compose
// stack actually runs against — see aeon-toolgw's policy_bundle mount) and reject manifests with
// real structural problems, not just superficial ones.
func TestAeonValidateAcceptsAndRejectsExampleManifests(t *testing.T) {
	root := repoRoot(t)
	withSchemasDir(t, filepath.Join(root, "proto"))

	t.Run("accepts every real example manifest", func(t *testing.T) {
		examples := []string{
			filepath.Join(root, "examples", "deep-research", "agent.yaml"),
			filepath.Join(root, "examples", "deep-research", "model_policy_bundle.yaml"),
			filepath.Join(root, "examples", "deep-research", "policy_bundle.yaml"),
		}
		for _, path := range examples {
			if err := validate(path); err != nil {
				t.Errorf("validate(%s) = %v, want nil (this file is the real config-as-code checked into the repo)", path, err)
			}
		}
	})

	t.Run("rejects a manifest with an unknown kind", func(t *testing.T) {
		path := writeTempManifest(t, "apiVersion: harness.ai/v1\nkind: NotARealKind\n")
		if err := validate(path); err == nil {
			t.Error("validate() = nil, want an error for an unrecognized kind")
		}
	})

	t.Run("rejects a manifest missing a required top-level field", func(t *testing.T) {
		path := writeTempManifest(t, "apiVersion: harness.ai/v1\nkind: Agent\nmetadata:\n  name: incomplete\n  version: 0.1.0\n")
		if err := validate(path); err == nil {
			t.Error("validate() = nil, want an error for an Agent manifest missing 'spec'")
		}
	})

	t.Run("rejects a ModelPolicyBundle whose profile fails the cross-file $ref to model_profile.schema.json", func(t *testing.T) {
		// Exercises the offline $ref resolution (model_policy_bundle.schema.json ->
		// model_profile.schema.json by $id) actually catching a real shape violation, not just
		// resolving without error.
		path := writeTempManifest(t, "apiVersion: harness.ai/v1\nkind: ModelPolicyBundle\nprofiles:\n  - name: x\n    capability: nonsense\n")
		if err := validate(path); err == nil {
			t.Error("validate() = nil, want an error for a profile that doesn't match model_profile.schema.json")
		}
	})
}

// TestAeonEvalListShowsSuitesFromEvalsDir is EVAL-001's acceptance test: `aeon eval list` must
// show the real suites checked into evals/suites — not a fixture, the same files the compose
// stack's Eval Runner (EVAL-002) will eventually read.
func TestAeonEvalListShowsSuitesFromEvalsDir(t *testing.T) {
	root := repoRoot(t)
	protoDir := filepath.Join(root, "proto")
	evalsDir := filepath.Join(root, "evals")

	t.Run("lists every real suite from examples/deep-research's evalGates", func(t *testing.T) {
		suites, err := listEvalSuites(evalsDir, protoDir)
		if err != nil {
			t.Fatalf("listEvalSuites: %v", err)
		}

		var buf bytes.Buffer
		printEvalSuites(&buf, suites)
		output := buf.String()

		// examples/deep-research/agent.yaml's evalGates names these four exactly — if any of
		// them stopped being a real, valid suite, that manifest's evalGates would reference a
		// suite that doesn't exist.
		for _, name := range []string{"deep_research_core", "citation_integrity", "injection_suite", "provider_conformance"} {
			if !bytes.Contains(buf.Bytes(), []byte(name)) {
				t.Errorf("aeon eval list output does not mention suite %q; output:\n%s", name, output)
			}
		}
	})

	t.Run("rejects a suite file that doesn't validate against eval_suite.schema.json", func(t *testing.T) {
		badEvalsDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(badEvalsDir, "suites"), 0o755); err != nil {
			t.Fatal(err)
		}
		badSuite := "apiVersion: harness.ai/v1\nkind: EvalSuite\nmetadata:\n  name: bad\n  version: 0.1.0\nspec:\n  dataset: x\n"
		if err := os.WriteFile(filepath.Join(badEvalsDir, "suites", "bad.yaml"), []byte(badSuite), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := listEvalSuites(badEvalsDir, protoDir); err == nil {
			t.Error("listEvalSuites() = nil error, want one for a suite missing required 'graders'/'thresholds'")
		}
	})
}

func writeTempManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp manifest: %v", err)
	}
	return path
}
