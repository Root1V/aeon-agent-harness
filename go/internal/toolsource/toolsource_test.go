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
		name: "an approval with no pinned descriptor",
		yaml: `kind: ToolSourceBundle
sources:
  - id: one
    endpoint: https://tools.example.test/mcp
    prefix: one.
    tools: [{name: t, side_effect: READ_ONLY, risk: low}]
`,
		want: "descriptor_sha256 must be",
		why:  "a pin that defaults to whatever arrives is not a pin, and this is the half that cannot be added later",
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

// TestTheShippedExampleIsInItsDocumentedBootstrapState keeps examples/deep-research/tool_sources.yaml
// from rotting into a file that asserts something the loader no longer does.
//
// IT IS EXPECTED TO BE REFUSED, and that is the point rather than a quirk: the example deliberately
// shows step 1 of the pinning flow — a source declared with no `descriptor_sha256` — because that is
// the state an operator starts in and the only way to learn the digest is to be refused and read the
// log. So the assertion is not "the example loads" but "the example parses, reaches validation, and
// is refused by the one rule it is demonstrating". A test that only checked it loads would force the
// example to carry invented digests, which is a worse document.
func TestTheShippedExampleIsInItsDocumentedBootstrapState(t *testing.T) {
	const path = "../../../examples/deep-research/tool_sources.yaml"
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the shipped example is gone: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("the example now loads: either its digests were filled in (then this test should assert " +
			"a successful load) or the pin stopped being required (then the example is teaching the wrong flow)")
	}
	if !strings.Contains(err.Error(), "descriptor_sha256 must be") {
		t.Errorf("the example is refused for a reason other than the missing pin it demonstrates: %v", err)
	}
}
