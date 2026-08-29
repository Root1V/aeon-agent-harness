package main

import (
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

func writeTempManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp manifest: %v", err)
	}
	return path
}
