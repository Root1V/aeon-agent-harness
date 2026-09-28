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
			Name:   "html-escaping-characters",
			Expect: expectHash,
			Why:    "Go's encoding/json HTML-escapes <, > and & by default; JCS does not. Two implementations of the same object would otherwise hash differently with no flag in sight.",
			Step: stepidentity.Step{StepID: "s1", ToolName: "search.web", ToolArgs: map[string]any{
				"query": "cats & dogs <tag>",
			}},
		},
		{
			Name:   "non-ascii-payload",
			Expect: expectHash,
			Why:    "Python's json.dumps escapes non-ASCII by default (ensure_ascii=True); JCS emits it raw as UTF-8. This is one of the two divergences that IS a flag, and it still has to be pinned.",
			Step: stepidentity.Step{StepID: "s2", ToolName: "search.web", ToolArgs: map[string]any{
				"query": "ñandú café 日本語",
			}},
		},
		{
			Name:   "integral-float-vs-int",
			Expect: expectHash,
			Why:    "Go writes 1 for a float64 1.0 and Python writes 1.0. NUMBERS ARE NOT A FLAG IN EITHER LANGUAGE, which is the reason a shared canonicalization had to be adopted rather than configured. JCS mandates ECMAScript number serialization, so both become 1.",
			Step: stepidentity.Step{StepID: "s3", ToolName: "payments.capture", ToolArgs: map[string]any{
				"amount": 1.0, "quantity": 1,
			}},
		},
		{
			Name:   "nested-object-key-order",
			Expect: expectHash,
			Why:    "Key order must be derived from the spec at every depth, not just at the top level. Keys are written here in deliberately wrong order so a canonicalizer that only sorts the outer object fails.",
			Step: stepidentity.Step{StepID: "s4", ToolName: "repository.read", ToolArgs: map[string]any{
				"zebra": map[string]any{"z": 1, "a": 2, "m": map[string]any{"y": 3, "b": 4}},
				"alpha": true,
			}},
		},
		{
			Name:   "empty-list-vs-absent",
			Expect: expectHash,
			Why:    "An empty list and an absent key are different objects and must hash differently. This is the pair that makes 'the harness sent no filters' distinguishable from 'the harness sent an empty filter set' — and an approval bound to one must not validate the other.",
			Step: stepidentity.Step{StepID: "s5", ToolName: "search.rag", ToolArgs: map[string]any{
				"query": "x", "filters": []any{},
			}},
		},
		{
			Name:   "absent-list-counterpart",
			Expect: expectHash,
			Why:    "The other half of empty-list-vs-absent. Two cases rather than one because a corpus that only contains the empty list cannot show that the absent one differs.",
			Step: stepidentity.Step{StepID: "s5", ToolName: "search.rag", ToolArgs: map[string]any{
				"query": "x",
			}},
		},
		{
			Name:   "key-outside-the-bmp",
			Expect: expectHash,
			Why:    "JCS sorts by UTF-16 code units, so a key outside the Basic Multilingual Plane is a surrogate pair and sorts by its surrogates — not by code point. An implementation sorting by code point diverges here and nowhere else.",
			Step: stepidentity.Step{StepID: "s6", ToolName: "artifact.read", ToolArgs: map[string]any{
				"\U0001F600": "emoji key", "z": "ascii key",
			}},
		},
		{
			Name:   "unicode-normalization-is-not-applied",
			Expect: expectHash,
			Why:    "Synaptum's addition. Precomposed 'é' (U+00E9) and 'e' + combining acute (U+0065 U+0301) look identical and are different strings. JCS orders by code units and DOES NOT normalize, so both keys coexist and sort apart. Fixed here rather than discovered later.",
			Step: stepidentity.Step{StepID: "s7", ToolName: "search.web", ToolArgs: map[string]any{
				"café":  "precomposed NFC",
				"café": "decomposed NFD",
			}},
		},
		{
			Name:   "big-integer-beyond-double-precision",
			Expect: expectReject,
			Why: "Synaptum's addition, and the most important case here. 2^53+1 is NOT representable as an " +
				"IEEE-754 double, and RFC 8785 mandates ECMAScript number serialization - whose numbers ARE " +
				"doubles. It used to canonicalize to 9007199254740992 and collide. SINCE 2026-09-27 IT IS " +
				"REFUSED, by agreement between all three teams.",
			RejectReason: "an integer of magnitude greater than 2^53 cannot be bound by this hash: two " +
				"different values canonicalize identically, so an approval granted for one would validate " +
				"the other. The implementation must RAISE rather than fold or preserve silently. Send such " +
				"an identifier as a string.",
			Step: stepidentity.Step{StepID: "s8", ToolName: "payments.capture", ToolArgs: map[string]any{
				"reference": json.Number("9007199254740993"),
			}},
			Notes: map[string]string{
				"why_refusing_beats_agreeing": "Folding consistently in all three implementations would have " +
					"restored equivalence and KEPT the collision: we would agree on the same wrong answer. " +
					"Refusing is what turns 'carry such an identifier as a string' from advice in a document " +
					"into something that cannot be broken without noticing.",
				"the_bound_is_a_clean_cut": "|v| > 2^53, NOT a round-trip check. Above 2^53 doubles step by " +
					"two, so every EVEN integer up there still round-trips exactly: a round-trip check " +
					"accepts half the identifiers anyone uses, BY PARITY - worse than no guard, because " +
					"whoever integrates it watches their test values pass. Both Aeon and Synaptum said the " +
					"round-trip version is what they would have written first. See big-integer-exact-but-refused.",
			},
		},
		{
			Name:   "big-integer-exact-but-refused",
			Expect: expectReject,
			Why: "2^53+2 IS exactly representable and round-trips perfectly, and it is refused anyway. This " +
				"case is the difference between the clean cut and the round-trip check, and it is what makes " +
				"the rule unambiguous to whoever implements it next.",
			RejectReason: "refused although exact: its neighbours 2^53+1 and 2^53+3 are not, and nobody can " +
				"reason about the parity of an identifier that does not exist yet.",
			Step: stepidentity.Step{StepID: "s8", ToolName: "payments.capture", ToolArgs: map[string]any{
				"reference": json.Number("9007199254740994"),
			}},
		},
		{
			Name:   "big-integer-at-the-boundary",
			Expect: expectHash,
			Why: "2^53 exactly, which MUST build. Synaptum asked for this one specifically and they were " +
				"right: a bound with no just-inside case is a bound nobody can tell is at 2^53 rather than " +
				"2^53-1, and either implementation could have been written with a >= without anything saying so.",
			Step: stepidentity.Step{StepID: "s8", ToolName: "payments.capture", ToolArgs: map[string]any{
				"reference": json.Number("9007199254740992"),
			}},
			Notes: map[string]string{
				"historical_collision": "Before 2026-09-27 a step with reference 9007199254740993 canonicalized " +
					"to exactly this case's canonical form - byte-identical, therefore the same sha256 - so an " +
					"approval granted for 9007199254740992 validated 9007199254740993. That pair can no longer " +
					"be produced, because the other half is now refused; this note is its only record.",
				"measured_folding": "9007199254740993 -> 9007199254740992 and 9007199254740995 -> " +
					"9007199254740996. The collisions are the ODD values. The first version of the pair in this " +
					"corpus did not collide at all, because it paired 2^53+1 with 2^53+2 - the intuitive choice, " +
					"and the wrong one.",
				"cross_language": "Synaptum's Python implementation did NOT reproduce the fold: Python keeps the " +
					"exact integer, so the same step produced two different hashes on the two sides. Worse than " +
					"the collision - on resume that is indistinguishable from someone mutating the arguments " +
					"after approval, a false alarm in the one place a false alarm costs attention.",
			},
		},
		{
			Name:   "same-args-different-step",
			Expect: expectHash,
			Why:    "The hole the triple closed: this is identical to html-escaping-characters except for step_id, and it MUST hash differently. Without step_id an approval for one step would validate the same tool with the same arguments at any other point in the run.",
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
		if c.Expect == expectReject {
			// Verified to be refused, and published with NO canonical form and NO hash. Inventing either
			// would be answering a question the implementation declines to answer.
			if _, err := c.Step.Hash(); err == nil {
				t.Fatalf("case %s is declared expect=reject and hashed cleanly", c.Name)
			}
			corpus.Cases = append(corpus.Cases, c)
			continue
		}
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

	// Every canonical form must be distinct, and there is NO LONGER A DECLARED COLLISION to exempt.
	//
	// That exemption existed for big-integer-collision, whose whole purpose was to hash identically to its
	// pair. Refusing integers above 2^53 removed the pair, so the collision cannot be produced at all any
	// more — and an exemption for a collision that can no longer happen is a hole kept open by habit, which
	// would forgive the next accidental duplicate. What survives of it is a NOTE on
	// big-integer-at-the-boundary, recording by hand what can no longer be generated.
	seen := map[string]string{}
	for _, c := range corpus.Cases {
		if c.Expect == expectReject {
			continue // No canonical form to compare; that is the point of the case.
		}
		if prev, dup := seen[c.Canonical]; dup {
			t.Fatalf("cases %q and %q hash identically — one of them is not testing what it claims, and no case "+
				"in this corpus is supposed to collide any more", prev, c.Name)
		}
		seen[c.Canonical] = c.Name
	}

	// A published rejection must actually be refused BY THIS IMPLEMENTATION. A corpus declaring a rule we do
	// not enforce would be asking the other two teams to implement it alone.
	rejections := 0
	for _, c := range corpus.Cases {
		if c.Expect != expectReject {
			continue
		}
		rejections++
		if c.RejectReason == "" {
			t.Fatalf("case %q is expect=reject with no reject_reason: the corpus would say this fails and not why", c.Name)
		}
	}
	if rejections == 0 {
		t.Fatal("no expect=reject case survived generation — the integer bound would be published as a rule with " +
			"nothing demonstrating it, which is how a bound becomes habit")
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
