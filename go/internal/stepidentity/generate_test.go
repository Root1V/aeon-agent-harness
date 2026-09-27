package stepidentity_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/stepidentity"
)

// TestGenerateGoldenCorpus writes the corpus, and only when asked.
//
// A test rather than a cmd/ binary so it shares the exact code path the verification uses — a separate
// generator is how a corpus and its checker drift into agreeing about different things. Guarded by an
// env var because a generator that runs on every `go test` would silently rewrite the contract to match
// whatever the code does today, which is the opposite of what a contract is for.
//
//	AEON_WRITE_STEP_IDENTITY_CORPUS=1 go test ./internal/stepidentity/ -run TestGenerateGoldenCorpus
func TestGenerateGoldenCorpus(t *testing.T) {
	if os.Getenv("AEON_WRITE_STEP_IDENTITY_CORPUS") == "" {
		t.Skip("set AEON_WRITE_STEP_IDENTITY_CORPUS=1 to rewrite the shared corpus")
	}

	// The cases the three teams agreed on: six I proposed, two Synaptum added, and one I measured.
	// Each carries WHY it is here, because a case without a stated property is a case nobody can tell
	// has stopped testing anything.
	cases := []goldenCase{
		{
			Name: "html-escaping-characters",
			Why:  "Go's encoding/json HTML-escapes <, > and & by default; JCS does not. Two implementations of the same object would otherwise hash differently with no flag in sight.",
			Step: stepidentity.Step{StepID: "s1", ToolName: "search.web", ToolArgs: map[string]any{
				"query": "cats & dogs <tag>",
			}},
		},
		{
			Name: "non-ascii-payload",
			Why:  "Python's json.dumps escapes non-ASCII by default (ensure_ascii=True); JCS emits it raw as UTF-8. This is one of the two divergences that IS a flag, and it still has to be pinned.",
			Step: stepidentity.Step{StepID: "s2", ToolName: "search.web", ToolArgs: map[string]any{
				"query": "ñandú café 日本語",
			}},
		},
		{
			Name: "integral-float-vs-int",
			Why:  "Go writes 1 for a float64 1.0 and Python writes 1.0. NUMBERS ARE NOT A FLAG IN EITHER LANGUAGE, which is the reason a shared canonicalization had to be adopted rather than configured. JCS mandates ECMAScript number serialization, so both become 1.",
			Step: stepidentity.Step{StepID: "s3", ToolName: "payments.capture", ToolArgs: map[string]any{
				"amount": 1.0, "quantity": 1,
			}},
		},
		{
			Name: "nested-object-key-order",
			Why:  "Key order must be derived from the spec at every depth, not just at the top level. Keys are written here in deliberately wrong order so a canonicalizer that only sorts the outer object fails.",
			Step: stepidentity.Step{StepID: "s4", ToolName: "repository.read", ToolArgs: map[string]any{
				"zebra": map[string]any{"z": 1, "a": 2, "m": map[string]any{"y": 3, "b": 4}},
				"alpha": true,
			}},
		},
		{
			Name: "empty-list-vs-absent",
			Why:  "An empty list and an absent key are different objects and must hash differently. This is the pair that makes 'the harness sent no filters' distinguishable from 'the harness sent an empty filter set' — and an approval bound to one must not validate the other.",
			Step: stepidentity.Step{StepID: "s5", ToolName: "search.rag", ToolArgs: map[string]any{
				"query": "x", "filters": []any{},
			}},
		},
		{
			Name: "absent-list-counterpart",
			Why:  "The other half of empty-list-vs-absent. Two cases rather than one because a corpus that only contains the empty list cannot show that the absent one differs.",
			Step: stepidentity.Step{StepID: "s5", ToolName: "search.rag", ToolArgs: map[string]any{
				"query": "x",
			}},
		},
		{
			Name: "key-outside-the-bmp",
			Why:  "JCS sorts by UTF-16 code units, so a key outside the Basic Multilingual Plane is a surrogate pair and sorts by its surrogates — not by code point. An implementation sorting by code point diverges here and nowhere else.",
			Step: stepidentity.Step{StepID: "s6", ToolName: "artifact.read", ToolArgs: map[string]any{
				"\U0001F600": "emoji key", "z": "ascii key",
			}},
		},
		{
			Name: "unicode-normalization-is-not-applied",
			Why:  "Synaptum's addition. Precomposed 'é' (U+00E9) and 'e' + combining acute (U+0065 U+0301) look identical and are different strings. JCS orders by code units and DOES NOT normalize, so both keys coexist and sort apart. Fixed here rather than discovered later.",
			Step: stepidentity.Step{StepID: "s7", ToolName: "search.web", ToolArgs: map[string]any{
				"café":  "precomposed NFC",
				"café": "decomposed NFD",
			}},
		},
		{
			Name: "big-integer-beyond-double-precision",
			Why: "Synaptum's addition, and it turned out to be the most important case here. 2^53+1 is NOT " +
				"representable as an IEEE-754 double, and RFC 8785 mandates ECMAScript number serialization — " +
				"whose numbers ARE doubles. So the canonical form of 9007199254740993 is 9007199254740992, and " +
				"that is the SPEC being correct, not an implementation losing a digit.",
			Step: stepidentity.Step{StepID: "s8", ToolName: "payments.capture", ToolArgs: map[string]any{
				"reference": json.Number("9007199254740993"),
			}},
			Notes: map[string]string{
				"consequence": "An approval CANNOT be bound to an argument carrying an integer above 2^53. " +
					"See big-integer-collision: two different references hash identically, so an approval granted " +
					"for one validates the other. Three implementations agreeing does not help — they agree on " +
					"the same wrong answer. Carry such a value as a STRING if it has to be bound.",
				"threshold": "2^53 = 9007199254740992 is the last CONSECUTIVE integer JCS round-trips exactly. " +
					"Above it even integers are still exact and odd ones are not, so the failure is intermittent by " +
					"value — which is worse than a clean cutoff, because half the test values anyone picks will work.",
			},
		},
		{
			Name: "big-integer-collision",
			Why: "The demonstration, and the reason the case above carries a warning rather than a note. This " +
				"step differs from big-integer-beyond-double-precision ONLY in its reference — 9007199254740992 " +
				"against 9007199254740993 — and both canonicalize to 9007199254740992, so an approval hash cannot " +
				"tell them apart.\n\n" +
				"The pair took two attempts to get right, which is worth recording because the wrong version is the " +
				"intuitive one. I first paired 2^53+1 with 2^53+2 and the collision did not happen: ABOVE 2^53 " +
				"doubles step by two, so every EVEN integer up there is still exact. 9007199254740994 round-trips " +
				"perfectly. The collisions are odd-numbered, and the first one is 2^53+1 folding onto 2^53. " +
				"Measured: 9007199254740993 -> 9007199254740992 and 9007199254740995 -> 9007199254740996.",
			Step: stepidentity.Step{StepID: "s8", ToolName: "payments.capture", ToolArgs: map[string]any{
				"reference": json.Number("9007199254740992"),
			}},
			Notes: map[string]string{
				"expected": "This case's sha256 is INTENTIONALLY identical to big-integer-beyond-double-precision. " +
					"It is the one pair in this corpus that must collide, and the uniqueness check below exempts it " +
					"by name for exactly that reason.",
			},
		},
		{
			Name: "same-args-different-step",
			Why:  "The hole the triple closed: this is identical to html-escaping-characters except for step_id, and it MUST hash differently. Without step_id an approval for one step would validate the same tool with the same arguments at any other point in the run.",
			Step: stepidentity.Step{StepID: "s1-bis", ToolName: "search.web", ToolArgs: map[string]any{
				"query": "cats & dogs <tag>",
			}},
		},
	}

	corpus := goldenCorpus{
		Canonicalization: "RFC 8785 (JCS), SHA-256 over the canonical bytes, hex lowercase",
		HashedShape:      `{"step_id": string, "tool_args": object, "tool_name": string} — a JSON OBJECT with exactly these three keys, canonicalized as a whole. JCS sorts the keys, so the order above is derived from the spec and not a convention any team has to remember.`,
	}
	for _, c := range cases {
		canonical, err := c.Step.CanonicalJSON()
		if err != nil {
			t.Fatalf("case %s: canonicalizing: %v", c.Name, err)
		}
		hash, err := c.Step.Hash()
		if err != nil {
			t.Fatalf("case %s: hashing: %v", c.Name, err)
		}
		c.Canonical = string(canonical)
		c.SHA256 = hash
		corpus.Cases = append(corpus.Cases, c)
	}

	// Every canonical form must be distinct. This is the assertion that would have caught the most
	// embarrassing possible bug in a corpus of hash cases: two cases that were meant to differ and do
	// not, sitting there passing forever.
	// The declared collision is exempt BY NAME, not by relaxing the rule. An exemption that silently
	// allowed any duplicate would defeat the check the moment a second, accidental one appeared.
	expectedCollisions := map[string]bool{"big-integer-collision": true}
	seen := map[string]string{}
	collisionsProven := 0
	for _, c := range corpus.Cases {
		prev, dup := seen[c.SHA256]
		switch {
		case dup && expectedCollisions[c.Name]:
			collisionsProven++
			t.Logf("declared collision confirmed: %q and %q share %s", prev, c.Name, c.SHA256)
		case dup:
			t.Fatalf("cases %q and %q hash identically — one of them is not testing what it claims", prev, c.Name)
		default:
			seen[c.SHA256] = c.Name
		}
	}
	if collisionsProven != len(expectedCollisions) {
		t.Fatalf("proved %d of %d declared collisions — big-integer-collision is supposed to COLLIDE, and if it "+
			"stopped colliding the warning attached to the case above would be documenting a limitation that no "+
			"longer exists", collisionsProven, len(expectedCollisions))
	}

	out, err := json.MarshalIndent(corpus, "", "  ")
	if err != nil {
		t.Fatalf("encoding corpus: %v", err)
	}
	out = append(out, '\n')
	path := corpusPath(t)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	t.Logf("wrote %d golden case(s) to %s", len(corpus.Cases), path)
}
