package toolexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testTenant is the tenant every call in this file acts for. Artifacts are parked per tenant since
// GOV-001g — `<root>/<tenant>/<id>` — so the fixture builds that shape rather than a flat directory,
// and the tool is handed the same per-tenant resolver cmd/aeon-toolgw builds.
const testTenant = "tenant-under-test"

func newArtifactFixture(t *testing.T) (*Executor, string, string) {
	t.Helper()
	base := t.TempDir()
	artifactDir := filepath.Join(base, "artifacts")
	outsideDir := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(artifactDir, testTenant), outsideDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(artifactDir, testTenant, "art_deadbeefcafe1234"), []byte("# Report\nthe findings\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("SECRET-OUTSIDE-THE-ROOT"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	e := &Executor{fns: map[string]ExecuteFunc{}}
	RegisterArtifactReadTool(e, func(tenant string) (*os.Root, error) {
		return os.OpenRoot(filepath.Join(artifactDir, tenant))
	}, artifactDir)
	return e, artifactDir, outsideDir
}

// TestArtifactReadReturnsWhatARunProduced is TOOL-009's happy path.
func TestArtifactReadReturnsWhatARunProduced(t *testing.T) {
	e, _, _ := newArtifactFixture(t)

	out, err := execTool(e, testTenant, "artifact.read", map[string]any{"artifact_id": "art_deadbeefcafe1234"})
	if err != nil {
		t.Fatalf("artifact.read: %v", err)
	}
	if got := out["content"]; got != "# Report\nthe findings\n" {
		t.Fatalf("content = %q", got)
	}
	if out["artifact_id"] != "art_deadbeefcafe1234" {
		t.Fatalf("the answer does not echo which artifact it is: %v", out)
	}
	if out["lines_total"] != 2 {
		t.Fatalf("lines_total = %v, want 2", out["lines_total"])
	}

	t.Run("a line range works here too", func(t *testing.T) {
		out, err := execTool(e, testTenant, "artifact.read", map[string]any{
			"artifact_id": "art_deadbeefcafe1234", "start_line": 2.0, "end_line": 2.0,
		})
		if err != nil {
			t.Fatalf("artifact.read: %v", err)
		}
		if got := out["content"]; got != "the findings" {
			t.Fatalf("content = %q", got)
		}
	})
}

// TestArtifactReadTakesAnIDAndNotAPath is the half that makes this tool different from
// repository.read pointed at another directory.
//
// An id has a known shape, so anything else is not a caller reaching for a file — it is a caller
// reaching for a path, most often a model that invented one. os.Root refuses every one of these on its
// own; the shape check is what makes the refusal say which kind of mistake it was, and it is asserted
// here so a future "just accept a relative path, it is contained anyway" cannot pass quietly.
func TestArtifactReadTakesAnIDAndNotAPath(t *testing.T) {
	e, artifactDir, outsideDir := newArtifactFixture(t)

	// A real file inside the root, reachable by name but NOT by a valid id: proof that the shape check
	// is doing work rather than restating what containment already guarantees.
	if err := os.WriteFile(filepath.Join(artifactDir, "notes.md"), []byte("inside but not an artifact"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(filepath.Join(outsideDir, "secret.txt"), // Inside the TENANT directory, which is the root the tool now opens — one level up it is
		// simply a file that is not there, and this subtest would stop testing containment.
		filepath.Join(artifactDir, testTenant, "art_00000000")); err != nil {
		t.Skipf("cannot create a symlink on this filesystem: %v", err)
	}

	for _, tc := range []struct {
		name, id, wantIn string
	}{
		{"a plain filename inside the root", "notes.md", "not an artifact id"},
		{"a traversal", "../outside/secret.txt", "not an artifact id"},
		{"an absolute path", filepath.Join(outsideDir, "secret.txt"), "not an artifact id"},
		{"a subdirectory", "sub/art_deadbeef", "not an artifact id"},
		{"the right prefix with the wrong alphabet", "art_NOT-HEX", "not an artifact id"},
		{"a bare prefix", "art_", "not an artifact id"},
		// This one has a VALID id and is a symlink out of the root: the shape check passes it and
		// os.Root stops it. Both guards are needed and this is the case that shows why.
		{"a valid id that is a symlink out of the root", "art_00000000", "escapes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execTool(e, testTenant, "artifact.read", map[string]any{"artifact_id": tc.id})
			if err == nil {
				t.Fatalf("the read SUCCEEDED and returned %v", out["content"])
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantIn)
			}
			if strings.Contains(err.Error(), "SECRET-OUTSIDE-THE-ROOT") {
				t.Fatalf("the contents leaked through the error: %v", err)
			}
		})
	}

	t.Run("a missing artifact_id is named", func(t *testing.T) {
		if _, err := execTool(e, testTenant, "artifact.read", map[string]any{}); err == nil {
			t.Fatal("a call with no artifact_id succeeded")
		}
	})

	t.Run("an id that is well-formed but absent reads as absent", func(t *testing.T) {
		_, err := execTool(e, testTenant, "artifact.read", map[string]any{"artifact_id": "art_ffffffffffff"})
		if err == nil {
			t.Fatal("reading an artifact that does not exist succeeded")
		}
		if strings.Contains(err.Error(), "not an artifact id") {
			t.Fatalf("a well-formed id was reported as malformed, which sends the caller to fix the wrong "+
				"thing: %v", err)
		}
	})
}

// execTool keeps these tests about the TOOL rather than about Execute's signature: INT-013 made
// Execute take an Invocation and return an Outcome so a recording could not be skipped, and every
// test here predates that and cares only about the result map.
func execTool(e *Executor, tenant, name string, args map[string]any) (map[string]any, error) {
	out, err := e.Execute(context.Background(), Invocation{Tenant: tenant, ToolName: name, Args: args})
	return out.Result, err
}
