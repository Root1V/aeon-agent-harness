package stepidentity_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowebpki/jcs"

	"github.com/aeon-ai/aeon/go/internal/stepidentity"
)

// corpusPath is the VENDORED copy of the golden corpus, inside this repository.
//
// It used to point straight at the shared coordination folder, outside the repo, which is where the
// contract is published for Synaptum and Axonium — and that is still where the source of truth lives.
// The consequence of reading it from there was measured rather than reasoned about, and it was worse
// than the note in backlog.md said: the test skipped on every machine that is not this laptop AND on
// this laptop too, because `make test-go-integration` runs the suite in a container that mounts
// /repo and nothing else, so the shared folder is outside the mount. A `no such file or directory`
// about a file that exists two directories away. **No target verified the contract**, including the
// one that looks most exhaustive — the same shape as the three gaps CI-001 found.
//
// So there are now two files and three tests, and the split is the whole point:
//
//	proto/contracts/.../hashes-dorados.json   this copy. Always present, so the contract is checked
//	                                          everywhere, including CI.
//	proto/contracts/.../procedencia.json      the upstream path and the upstream file's sha256 at the
//	                                          moment it was vendored.
//
// TestStepIdentityMatchesGoldenCorpus checks the implementation against this copy.
// TestTheVendoredCorpusIsTheOneThatWasReviewed hashes this copy against the recorded sha256, so
// editing it here to make a failing test pass is itself a failure, and it runs everywhere.
// TestTheVendoredCorpusHasNotDriftedFromTheSharedOne compares it byte for byte against upstream
// wherever that folder is reachable. Without that third one, a copy that falls behind looks exactly
// like a verified one — which is the defect family this corpus exists to prevent, so reintroducing it
// as the price of fixing the skip would have been a bad trade.
//
// AEON_STEP_IDENTITY_CORPUS still overrides, for checking an implementation against some other file.
func corpusPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("AEON_STEP_IDENTITY_CORPUS"); p != "" {
		return p
	}
	return filepath.Join("..", "..", "..", "proto", "contracts", "identidad-de-paso", "hashes-dorados.json")
}

// provenancePath is corpusPath's companion: where this copy came from, and its hash when it did.
func provenancePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "proto", "contracts", "identidad-de-paso", "procedencia.json")
}

// sharedCorpusPath is the SOURCE OF TRUTH, in the three teams' coordination folder. Reachable only
// where that folder is mounted — this laptop, and the integration target now mounts it when it exists.
// AEON_STEP_IDENTITY_SHARED_CORPUS overrides it, which is how the container is told where it landed.
func sharedCorpusPath() string {
	if p := os.Getenv("AEON_STEP_IDENTITY_SHARED_CORPUS"); p != "" {
		return p
	}
	return filepath.Join("..", "..", "..", "..", "..", "Victor", "coordinacion_project",
		"contratos", "identidad-de-paso", "fixtures", "hashes-dorados.json")
}

type corpusProvenance struct {
	UpstreamPath string `json:"upstream_path"`
	SHA256       string `json:"sha256"`
	VendoredAt   string `json:"vendored_at"`
}

func readProvenance(t *testing.T) corpusProvenance {
	t.Helper()
	raw, err := os.ReadFile(provenancePath(t))
	if err != nil {
		t.Fatalf("reading the vendored corpus's provenance: %v — without it this copy is a file of "+
			"unknown origin, and the tests below would be checking the implementation against itself", err)
	}
	var p corpusProvenance
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("parsing the provenance: %v", err)
	}
	if p.SHA256 == "" || p.UpstreamPath == "" {
		t.Fatalf("the provenance names no %s", map[bool]string{true: "sha256", false: "upstream_path"}[p.SHA256 == ""])
	}
	return p
}

// TestTheVendoredCorpusIsTheOneThatWasReviewed runs EVERYWHERE, and it is what makes the vendored copy
// worth anything in CI: it proves the bytes being checked are the bytes that were taken from the shared
// contract, not bytes somebody adjusted locally until the suite went green. The cheapest way to make a
// contract test pass is to edit the contract, and that is exactly what this refuses.
func TestTheVendoredCorpusIsTheOneThatWasReviewed(t *testing.T) {
	prov := readProvenance(t)
	raw, err := os.ReadFile(corpusPath(t))
	if err != nil {
		t.Fatalf("reading the vendored corpus: %v", err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != prov.SHA256 {
		t.Fatalf("the vendored corpus does not hash to its recorded provenance.\n"+
			" got: %s\nwant: %s\n\nEither this copy was edited in place — which is how a contract test is "+
			"made to pass by changing the contract — or it was refreshed without updating procedencia.json. "+
			"Refresh both together: see the `refresh` field in that file.", got, prov.SHA256)
	}
	t.Logf("vendored corpus matches its provenance (%s, vendored %s from %s)", prov.SHA256[:12], prov.VendoredAt, prov.UpstreamPath)
}

// TestTheVendoredCorpusHasNotDriftedFromTheSharedOne is the one test here that is allowed to skip, and
// what it skips on is narrow and honest: whether the three teams' coordination folder is on this
// machine. The CONTRACT is verified without it; what needs it is the question of whether our copy is
// still the current one.
//
// It fails on any difference rather than reporting one, because the failure mode it guards is silent by
// construction: a stale copy passes every other test in this file. Synaptum and Axonium were told this
// trade-off when the copy was made.
func TestTheVendoredCorpusHasNotDriftedFromTheSharedOne(t *testing.T) {
	shared, err := os.ReadFile(sharedCorpusPath())
	if err != nil {
		t.Skipf("the shared contract folder is not on this machine (%v) — the vendored copy is still "+
			"verified against its provenance and against this implementation; what cannot be checked "+
			"here is whether upstream has moved", err)
	}
	vendored, err := os.ReadFile(corpusPath(t))
	if err != nil {
		t.Fatalf("reading the vendored corpus: %v", err)
	}
	if !bytes.Equal(shared, vendored) {
		sharedSum := sha256.Sum256(shared)
		vendoredSum := sha256.Sum256(vendored)
		t.Fatalf("the vendored corpus has drifted from the shared contract.\n"+
			" shared  %s (%d bytes) %s\n vendored %s (%d bytes) %s\n\n"+
			"The shared file is the contract; this copy is not. Refresh it and its provenance together "+
			"(see the `refresh` field in procedencia.json) and tell the other two teams what changed.",
			hex.EncodeToString(sharedSum[:])[:12], len(shared), sharedCorpusPath(),
			hex.EncodeToString(vendoredSum[:])[:12], len(vendored), corpusPath(t))
	}
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
			// TWO FORMS OF THE SAME EXPECTATION, and reading both is what removed a skip rather
			// than tolerating one. The library ships `outhex/<name>.txt` (the canonical bytes as
			// hex) for nine vectors and `output/<name>` (the canonical JSON itself) for all ten:
			// simpleString.json has only the second. The previous version looked for the hex alone
			// and skipped with "no expected output for simpleString.json" — which read as the
			// vector being unverifiable, when the expectation was in the next directory along.
			// A skip whose stated reason is not the real one is the kind this repo keeps finding.
			var want []byte
			base := strings.TrimSuffix(name, ".json")
			if wantHex, err := os.ReadFile(filepath.Join(root, "outhex", base+".txt")); err == nil {
				want, err = hex.DecodeString(strings.Join(strings.Fields(string(wantHex)), ""))
				if err != nil {
					t.Fatalf("decoding expected hex for %s: %v", name, err)
				}
			} else if wantRaw, err := os.ReadFile(filepath.Join(root, "output", name)); err == nil {
				want = wantRaw
			} else {
				t.Fatalf("%s has neither outhex/%s.txt nor output/%s — this vector cannot be "+
					"verified, and the anchor of the corpus's credibility must not quietly shrink", name, base, name)
			}

			got, err := jcs.Transform(raw)
			if err != nil {
				t.Fatalf("canonicalizing %s: %v", name, err)
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
//
// IT ASKS THE GO TOOLCHAIN instead of guessing, and the guess is why the anchor of this corpus's
// whole chain of trust had never run anywhere but one laptop. The previous version read the
// GOMODCACHE *environment variable* — which is empty on a normal machine, because GOMODCACHE is a
// value `go env` COMPUTES — and fell back to $HOME/go/pkg/mod. On this laptop that is correct by
// coincidence. In the golang:1.25-alpine image the integration target uses, GOPATH is /go, so the
// cache is /go/pkg/mod and $HOME/go/pkg/mod does not exist at all: the test skipped with "the jcs
// module's RFC testdata is not on this machine" while the testdata was sitting right there, measured
// at /go/pkg/mod/github.com/gowebpki/jcs@v1.0.2/testdata/input.
//
// That mattered more than the skip count suggested. This test is what makes the corpus credible to
// the other two teams — without it the chain starts at a library agreeing with itself — so the piece
// of evidence Synaptum explicitly asked for was the one piece no automated target checked.
func rfcVectorDir(t *testing.T) string {
	t.Helper()
	gomodcache := os.Getenv("GOMODCACHE")
	if gomodcache == "" {
		if out, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
			gomodcache = strings.TrimSpace(string(out))
		}
	}
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
