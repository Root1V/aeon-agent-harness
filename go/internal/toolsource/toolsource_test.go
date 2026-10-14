package toolsource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheBundleRefusesWhatWouldBeUnsafeOrAmbiguous covers the guards an operator meets, each of
// which exists because of a failure this repository has already paid for once.
func TestTheBundleRefusesWhatWouldBeUnsafeOrAmbiguous(t *testing.T) {
	const okDigest = "0000000000000000000000000000000000000000000000000000000000000000"

	cases := []struct {
		name string
		yaml string
		want string
		why  string
	}{{
		name: "a bundle that declares nothing",
		yaml: "kind: ToolSourceBundle\nsources: []\n",
		want: "no sources declared",
		why:  "the policy loader shipped a bundle whose key was misspelled: it parsed into zero policies and denied everything",
	}, {
		name: "the wrong kind",
		yaml: "kind: CallerBundle\nsources: []\n",
		want: "kind is",
		why:  "YAML drops keys it does not know, so the kind is the only thing that catches the wrong file",
	}, {
		name: "two sources sharing a prefix",
		yaml: `kind: ToolSourceBundle
sources:
  - id: one
    endpoint: https://a.test/mcp
    prefix: shared.
    tools: [{name: t, side_effect: READ_ONLY, risk: low, descriptor_sha256: "` + okDigest + `"}]
  - id: two
    endpoint: https://b.test/mcp
    prefix: shared.
    tools: [{name: t, side_effect: READ_ONLY, risk: low, descriptor_sha256: "` + okDigest + `"}]
`,
		want: "is already used by source",
		why:  "the catalogue is keyed by tool name, so a shared prefix collides in silence with the last refresh winning",
	}, {
		name: "a plaintext endpoint to a non-loopback host",
		yaml: `kind: ToolSourceBundle
sources:
  - id: one
    endpoint: http://tools.example.test/mcp
    prefix: one.
    tools: [{name: t, side_effect: READ_ONLY, risk: low, descriptor_sha256: "` + okDigest + `"}]
`,
		want: "plaintext http",
		why:  "a bearer token would go over the wire in the clear, and a plaintext endpoint works fine while being readable by anyone on the path",
	}, {
		name: "a secret pasted where a variable NAME belongs",
		yaml: `kind: ToolSourceBundle
sources:
  - id: one
    endpoint: https://tools.example.test/mcp
    prefix: one.
    auth:
      token_url: https://tools.example.test/oauth2/token
      client_id_env: AEON_SOURCE_CLIENT_ID
      client_secret_env: "not-a-variable-name-but-a-value"
    tools: [{name: t, side_effect: READ_ONLY, risk: low, descriptor_sha256: "` + okDigest + `"}]
`,
		want: "must be the NAME of an environment variable",
		why:  "this file is meant to be committed, and the likeliest way to get the field wrong is to paste the secret",
	}, {
		name: "effects without an idempotency key",
		yaml: `kind: ToolSourceBundle
sources:
  - id: one
    endpoint: https://tools.example.test/mcp
    prefix: one.
    tools: [{name: t, side_effect: WRITE, risk: high, descriptor_sha256: "` + okDigest + `"}]
`,
		want: "needs idempotency_key_fields",
		why:  "the registry's own rule (ADR-0001), checked here so it fails at startup rather than at the first call",
	}, {
		name: "no classification at all",
		yaml: `kind: ToolSourceBundle
sources:
  - id: one
    endpoint: https://tools.example.test/mcp
    prefix: one.
    tools: [{name: t, descriptor_sha256: "` + okDigest + `"}]
`,
		want: "side_effect and risk are required",
		why:  "tools/list reports neither, and guessing turns a tool with effects into a read",
	}, {
		name: "a pin that is not a digest at all",
		yaml: `kind: ToolSourceBundle
sources:
  - id: one
    endpoint: https://tools.example.test/mcp
    prefix: one.
    tools: [{name: t, side_effect: READ_ONLY, risk: low, descriptor_sha256: "probably-fine"}]
`,
		want: "not a 64-character hex",
		why:  "a pin that is not a digest can never match, so it would silently mean 'never serve this'",
	}, {
		name: "a source that approves nothing",
		yaml: `kind: ToolSourceBundle
sources:
  - id: one
    endpoint: https://tools.example.test/mcp
    prefix: one.
    tools: []
`,
		want: "approves no tools",
		why:  "it would be connected to and serve nothing, which is a mistake more often than an intention",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sources.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("accepted — %s", tc.why)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refused for the wrong reason: got %q, want it to mention %q", err, tc.want)
			}
		})
	}

	// AN EMPTY PIN IS ACCEPTED, and this used to be refused. Veritium followed our documented flow —
	// declare the tool, start once, read the digest from the log — and the gateway exited inside
	// Load, before connecting, so the log never ran. The tool is still not served; the only thing
	// that changed is that an operator can now reach the digest.
	t.Run("an empty pin loads, because that is how an operator reaches the digest", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sources.yaml")
		body := `kind: ToolSourceBundle
sources:
  - id: one
    endpoint: https://tools.example.test/mcp
    prefix: one.
    tools: [{name: t, side_effect: READ_ONLY, risk: low}]
`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		b, err := Load(path)
		if err != nil {
			t.Fatalf("an unapproved tool made the whole bundle unloadable: %v", err)
		}
		if b.Sources[0].Tools[0].Approved() {
			t.Error("a tool with no digest reports itself approved")
		}
	})

	t.Run("plaintext to a non-loopback host needs an explicit, logged decision", func(t *testing.T) {
		body := func(allow string) string {
			return `kind: ToolSourceBundle
sources:
  - id: hostside
    endpoint: http://host.docker.internal:8010/mcp
    prefix: hostside.
` + allow + `    tools: [{name: t, side_effect: READ_ONLY, risk: low, descriptor_sha256: "` + okDigest + `"}]
`
		}
		path := filepath.Join(t.TempDir(), "no.yaml")
		if err := os.WriteFile(path, []byte(body("")), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := Load(path); err == nil {
			t.Error("plaintext to a non-loopback host was accepted without anybody deciding it")
		} else if !strings.Contains(err.Error(), "allow_plain_http") {
			t.Errorf("the refusal does not name the way out: %v", err)
		}

		path = filepath.Join(t.TempDir(), "yes.yaml")
		if err := os.WriteFile(path, []byte(body("    allow_plain_http: true\n")), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		b, err := Load(path)
		if err != nil {
			t.Fatalf("an explicit allow_plain_http was still refused: %v", err)
		}
		// AND IT IS LOUD. A waived security rule that nobody sees again is a rule that was removed,
		// so the bundle reports it for the binary to log on every start.
		warnings := b.PlainHTTPWarnings()
		if len(warnings) != 1 || !strings.Contains(warnings[0], "host.docker.internal:8010") {
			t.Errorf("warnings = %v, want one naming the source and host", warnings)
		}
	})

	t.Run("loopback plaintext is allowed, because that is where every test of this runs", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sources.yaml")
		body := `kind: ToolSourceBundle
sources:
  - id: local
    endpoint: http://127.0.0.1:9123/mcp
    prefix: local.
    tools: [{name: t, side_effect: READ_ONLY, risk: low, descriptor_sha256: "` + okDigest + `"}]
`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := Load(path); err != nil {
			t.Fatalf("a loopback source was refused: %v", err)
		}
	})
}

// TestTheFingerprintIsStableAcrossKeyOrder is why RFC 8785 is used rather than encoding/json: a
// schema arriving from the wire is a map, and without canonicalization a source that reordered its
// keys would look like drift on every refresh — which would train an operator to re-paste digests
// without reading what changed.
func TestTheFingerprintIsStableAcrossKeyOrder(t *testing.T) {
	a := map[string]any{"type": "object", "properties": map[string]any{
		"alpha": map[string]any{"type": "string"}, "beta": map[string]any{"type": "number"},
	}}
	b := map[string]any{"properties": map[string]any{
		"beta": map[string]any{"type": "number"}, "alpha": map[string]any{"type": "string"},
	}, "type": "object"}

	da, err := DescriptorFingerprint("t", "d", a)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	db, err := DescriptorFingerprint("t", "d", b)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if da != db {
		t.Errorf("the same schema in a different key order hashes differently:\n  %s\n  %s", da, db)
	}

	// And a REAL change still changes it, or the stability above would be indistinguishable from a
	// constant.
	changed := map[string]any{"type": "object", "properties": map[string]any{
		"alpha": map[string]any{"type": "string"}, "beta": map[string]any{"type": "number"},
		"delete_after": map[string]any{"type": "boolean"},
	}}
	dc, err := DescriptorFingerprint("t", "d", changed)
	if err != nil {
		t.Fatalf("changed: %v", err)
	}
	if dc == da {
		t.Error("adding an argument did not change the digest — the pin would never catch drift")
	}
}

// TestTheShippedExampleIsInItsDocumentedBootstrapState keeps
// examples/deep-research/tool_sources.yaml from rotting into a file that asserts something the
// loader no longer does.
//
// IT MUST LOAD, AND ITS TOOLS MUST BE UNAPPROVED. The example shows step 1 of the pinning flow — a
// source declared with no digest — and the whole point of TOOL-010b is that this state now STARTS
// the gateway instead of refusing to. The earlier version of this test asserted the opposite and
// was right at the time; it is flipped here deliberately rather than deleted, because the behaviour
// it pinned is exactly what Veritium reported as broken.
func TestTheShippedExampleIsInItsDocumentedBootstrapState(t *testing.T) {
	const path = "../../../examples/deep-research/tool_sources.yaml"
	b, err := Load(path)
	if err != nil {
		t.Fatalf("the shipped example does not load, so it teaches a flow that does not work: %v", err)
	}
	if len(b.Sources) == 0 {
		t.Fatal("the example declares no sources")
	}
	for _, src := range b.Sources {
		for _, tool := range src.Tools {
			if tool.Approved() {
				t.Errorf("example tool %q carries a digest: the example is meant to show the state BEFORE a "+
					"person has reviewed the descriptor", tool.Name)
			}
		}
	}
}
