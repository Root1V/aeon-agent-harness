package stepidentity_test

import (
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
	Name      string            `json:"name"`
	Why       string            `json:"why"`
	Step      stepidentity.Step `json:"step"`
	Canonical string            `json:"canonical_json"`
	SHA256    string            `json:"sha256"`
	Notes     map[string]string `json:"notes,omitempty"`
}

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
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parsing the corpus: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("the corpus has no cases")
	}

	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
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
