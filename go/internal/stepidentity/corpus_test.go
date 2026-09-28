package stepidentity_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowebpki/jcs"

	"github.com/aeon-ai/aeon/go/internal/stepidentity"
)

// corpusPath is where the golden corpus is published for the other two teams. Outside this repo on
// purpose: it is a shared artifact, and the coordination folder is where the three teams already look.
// AEON_STEP_IDENTITY_CORPUS overrides it so the test runs in a container that has not mounted it.
func corpusPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("AEON_STEP_IDENTITY_CORPUS"); p != "" {
		return p
	}
	return filepath.Join("..", "..", "..", "..", "..", "Victor", "coordinacion_project",
		"contratos", "identidad-de-paso", "fixtures", "hashes-dorados.json")
}

type goldenCase struct {
	Name string            `json:"name"`
	Why  string            `json:"why"`
	Step stepidentity.Step `json:"step"`
	// Expect is "hash" or "reject" (agreed with Synaptum, 2026-09-27). Before it existed, every case
	// carried a sha256 and there was no way to express a case that must FAIL TO BUILD — which is what the
	// integer bound needs, since the whole point is that such a step has no hash.
	Expect string `json:"expect"`
	// RejectReason is REQUIRED when Expect is "reject", at Synaptum's request and for a good reason: a case
	// that only says "this fails" turns the corpus into a list of prohibitions with no argument, and in six
	// months nobody knows whether the bound sits at 2^53 for a reason or out of habit.
	RejectReason string `json:"reject_reason,omitempty"`
	// Canonical and SHA256 are empty for a rejected case. There is no canonical form of a step that cannot
	// be built, and writing one would be inventing the answer to a question the implementation refuses.
	Canonical string            `json:"canonical_json,omitempty"`
	SHA256    string            `json:"sha256,omitempty"`
	Notes     map[string]string `json:"notes,omitempty"`
}

// expectHash and expectReject are the two values of Expect.
const (
	expectHash   = "hash"
	expectReject = "reject"
)

type goldenCorpus struct {
	Canonicalization string       `json:"canonicalization"`
	HashedShape      string       `json:"hashed_shape"`
	Cases            []goldenCase `json:"cases"`
}

// TestGoldenCorpusMatchesRFCVectors anchors the chain of trust, and it runs FIRST for a reason.
//
// The corpus this package publishes is only worth something if its values do not come from the same
// code they are meant to check. Synaptum said exactly that when they asked for it: "no me fiaría de la
// especificación tampoco". So the chain is:
//
//	RFC 8785's own published vectors
//	  -> verify github.com/gowebpki/jcs (the WebPKI group's implementation, same authors as the RFC)
//	  -> that library canonicalizes our corpus
//	  -> Synaptum and Axonium check their implementations against the corpus
//
// Without this test the chain starts at a library agreeing with itself, and the corpus would prove
// nothing to anyone. The vectors are the ones shipped in the library's own testdata, which are the
// RFC's — including french.json, RFC 8785's sorting example, where "péché" must sort AFTER "peach"
// because JCS orders by UTF-16 code units and not by locale.
func TestGoldenCorpusMatchesRFCVectors(t *testing.T) {
	root := rfcVectorDir(t)
	if root == "" {
		t.Skip("the jcs module's RFC testdata is not on this machine — see GOMODCACHE")
	}

	inputs, err := os.ReadDir(filepath.Join(root, "input"))
	if err != nil {
		t.Fatalf("reading RFC vectors: %v", err)
	}
	if len(inputs) == 0 {
		t.Fatal("no RFC vectors found — the anchor of this corpus's credibility is missing, so nothing below means anything")
	}

	checked := 0
	for _, entry := range inputs {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, "input", name))
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			wantHex, err := os.ReadFile(filepath.Join(root, "outhex", strings.TrimSuffix(name, ".json")+".txt"))
			if err != nil {
				t.Skipf("no expected output for %s", name)
			}

			got, err := jcs.Transform(raw)
			if err != nil {
				t.Fatalf("canonicalizing %s: %v", name, err)
			}
			want, err := hex.DecodeString(strings.Join(strings.Fields(string(wantHex)), ""))
			if err != nil {
				t.Fatalf("decoding expected hex for %s: %v", name, err)
			}
			if string(got) != string(want) {
				t.Errorf("canonicalization of %s does not match the RFC vector\n got: %q\nwant: %q", name, got, want)
			}
		})
		checked++
	}
	t.Logf("verified the canonicalizer against %d RFC 8785 vector(s)", checked)
}

// rfcVectorDir locates the jcs module's testdata in the module cache.
func rfcVectorDir(t *testing.T) string {
	t.Helper()
	gomodcache := os.Getenv("GOMODCACHE")
	if gomodcache == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		gomodcache = filepath.Join(home, "go", "pkg", "mod")
	}
	matches, err := filepath.Glob(filepath.Join(gomodcache, "github.com", "gowebpki", "jcs@*", "testdata"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// TestStepIdentityMatchesGoldenCorpus checks this implementation against the published corpus.
//
// The corpus file is the CONTRACT and this code is one implementation of it — that direction matters.
// If the test regenerated the values it would pass by construction, and the file would document
// whatever the code happened to do rather than constrain it.
func TestStepIdentityMatchesGoldenCorpus(t *testing.T) {
	raw, err := os.ReadFile(corpusPath(t))
	if err != nil {
		t.Skipf("golden corpus not readable (%v) — it lives in the shared coordination folder", err)
	}
	var corpus goldenCorpus
	// DECODED WITH UseNumber, and this is not a detail — it is the Go-specific half of the whole integer
	// bound, discovered by this very test failing.
	//
	// encoding/json turns a JSON number into float64 unless asked otherwise, so `9007199254740993` in the
	// corpus file arrives here as 9007199254740992.0: FOLDED BEFORE the bound can look at it, and therefore
	// accepted. The reject case reported "expected to be REFUSED and it hashed cleanly" — the guard was
	// blind to the exact value it exists to refuse, because the loss happened one layer earlier.
	//
	// The consequence reaches past this test and is worth stating plainly: in Go the bound only protects a
	// caller who decodes with UseNumber. A caller using plain json.Unmarshal hands us a value whose digits
	// are already gone, and no check downstream can recover them. That is the mirror image of Synaptum's
	// situation, where Python keeps arbitrary-precision integers and the risk is losing them on the way OUT.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&corpus); err != nil {
		t.Fatalf("parsing the corpus: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("the corpus has no cases")
	}

	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			// An unknown or missing `expect` FAILS rather than defaulting to "hash". A corpus consumer that
			// defaulted would silently treat a reject case as a hash case and report a passing verification
			// of the opposite property — the same shape as the normalization runner that read 5 of 13 keys.
			switch c.Expect {
			case expectHash, expectReject:
			default:
				t.Fatalf("case declares expect=%q, which this runner does not implement — a case whose "+
					"expectation cannot be read must not be evaluated as though it could", c.Expect)
			}

			if c.Expect == expectReject {
				if c.RejectReason == "" {
					t.Error("a reject case with no reject_reason: the corpus would say this fails and not why, " +
						"and in six months nobody could tell whether the bound is principled or habitual")
				}
				if c.SHA256 != "" || c.Canonical != "" {
					t.Errorf("a reject case carries a hash (%q) — there is no canonical form of a step that "+
						"cannot be built, and publishing one invents the answer to a refused question", c.SHA256)
				}
				if _, err := c.Step.Hash(); err == nil {
					t.Fatal("this step was expected to be REFUSED and it hashed cleanly")
				}
				return
			}

			canonical, err := c.Step.CanonicalJSON()
			if err != nil {
				t.Fatalf("canonicalizing: %v", err)
			}
			if string(canonical) != c.Canonical {
				// The bytes, not just the hash: two different hashes say nothing about where two
				// implementations parted company, and two byte sequences say exactly where.
				t.Errorf("canonical JSON differs\n got: %s\nwant: %s", canonical, c.Canonical)
			}
			got, err := c.Step.Hash()
			if err != nil {
				t.Fatalf("hashing: %v", err)
			}
			if got != c.SHA256 {
				t.Errorf("sha256 = %s, want %s (%s)", got, c.SHA256, c.Why)
			}
		})
	}
	t.Logf("verified %d golden case(s) from %s", len(corpus.Cases), corpus.Canonicalization)
}

// TestTheFloatBoundIsInclusiveAndWhy pins the asymmetry between the integer rule and the float rule.
//
// The bound is `> 2^53` for an integer and `>= 2^53` FOR A FLOAT, and the difference is about provenance
// rather than magnitude. With an integer there is no ambiguity: 2^53 is 2^53. With a float there is no way
// to know where the value came from, because 9007199254740993.0 parses to exactly 9007199254740992.0 — so
// binding it would bind two different values to one hash, which is the whole thing this check prevents.
//
// HOW THIS WAS FOUND, because it is the useful part: Synaptum found it on THEIR side on 2026-09-28, from
// the warning we sent about Go and Python losing digits at opposite ends. The same hole was in this code —
// a float literal above 2^53 passed a `> 2^53` cut and got hashed. Neither of us found it by reasoning
// about the rule; both found it by running a literal through it.
//
// An earlier version of this test asserted the OPPOSITE — that a float64-decoded 2^53+1 was accepted, and
// documented that as an acceptable limit of the Go side. It was not acceptable; it was the hole.
func TestTheFloatBoundIsInclusiveAndWhy(t *testing.T) {
	hashOf := func(t *testing.T, body string, useNumber bool) (string, error) {
		t.Helper()
		var args map[string]any
		if useNumber {
			dec := json.NewDecoder(bytes.NewReader([]byte(body)))
			dec.UseNumber()
			if err := dec.Decode(&args); err != nil {
				t.Fatal(err)
			}
		} else if err := json.Unmarshal([]byte(body), &args); err != nil {
			t.Fatal(err)
		}
		return stepidentity.Step{StepID: "s", ToolName: "payments.capture", ToolArgs: args}.Hash()
	}

	t.Run("a float literal above 2^53 is refused, digits already gone or not", func(t *testing.T) {
		// The hole itself: this parses to exactly 2^53 and used to hash as the boundary value.
		for _, useNumber := range []bool{true, false} {
			if _, err := hashOf(t, `{"reference": 9007199254740993.0}`, useNumber); err == nil {
				t.Errorf("useNumber=%v: a float above 2^53 was accepted — it parses to exactly 2^53, so it would "+
					"share a hash with a different value", useNumber)
			}
		}
	})

	t.Run("a float AT 2^53 is refused too, although it looks exact", func(t *testing.T) {
		if _, err := hashOf(t, `{"reference": 9007199254740992.0}`, true); err == nil {
			t.Error("a float at 2^53 was accepted — indistinguishable from a folded 2^53+1, which is the point")
		}
	})

	t.Run("the same value as an INTEGER at 2^53 is accepted", func(t *testing.T) {
		// The asymmetry, asserted. Without this the two rules could be collapsed into one by anyone tidying
		// up, and the corpus case big-integer-at-the-boundary would stop being reachable.
		if _, err := hashOf(t, `{"reference": 9007199254740992}`, true); err != nil {
			t.Errorf("an integer at 2^53 was refused: %v — 2^53 is exactly representable and unambiguous", err)
		}
	})

	t.Run("ordinary fractions still pass", func(t *testing.T) {
		// The bug the first version of the float branch introduced: it refused EVERY json.Number that was not
		// a valid int64, so 1.5 came back as "too large to bind". The bound is about integers used as
		// identifiers, and refusing a price would have been found by the first caller rather than by a test.
		for _, body := range []string{`{"amount": 1.5}`, `{"amount": 0.1}`, `{"amount": 1.0}`} {
			if _, err := hashOf(t, body, true); err != nil {
				t.Errorf("%s was refused: %v", body, err)
			}
		}
	})

	t.Run("without UseNumber the error is CONSERVATIVE, not permissive", func(t *testing.T) {
		// What remains of the Go-specific asymmetry, and its direction now matters: a caller who decodes
		// without UseNumber hands us a float64, so even a legitimate integer 2^53 is refused. That is a false
		// refusal — annoying, visible, and fixable by the caller. It is the opposite of the old behaviour,
		// which silently accepted a value whose digits were already lost.
		if _, err := hashOf(t, `{"reference": 9007199254740992}`, false); err == nil {
			t.Error("an integer 2^53 decoded as float64 was accepted — then the Go side is permissive again " +
				"exactly where it cannot tell what it was given")
		}
	})
}
