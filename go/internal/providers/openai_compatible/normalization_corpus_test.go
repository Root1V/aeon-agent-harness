package openaicompatible

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/providers"
)

// The normalization corpus is the shared contract published by the Synaptum team (H1 = D: specify
// once, implement per language). See evals/contracts/normalizacion/spec.md.
type normCorpus struct {
	Contract string     `json:"contract"`
	Version  string     `json:"version"`
	Dialect  string     `json:"dialect"`
	BodyRoot string     `json:"body_root"`
	Cases    []normCase `json:"cases"`
	// BodyManifestVersion is the version of the body corpus these expectations were written
	// against. Checking it is not bookkeeping: the bodies live in a different published corpus and
	// are re-recorded independently, so a stale vendored copy pairs today's expectations with
	// yesterday's bodies and produces results that look real. This runner's first outing did
	// exactly that and nobody noticed, including the runner.
	BodyManifestVersion int `json:"body_manifest_version"`
	// AuthoredRoot holds bodies written by hand rather than recorded. They exist because some
	// properties stopped having a real recording — after the v8 re-record every Prometheus response
	// carries usage, so "nobody measured" had nowhere left to come from. Keeping them in their own
	// directory, flagged per case, is what stops a hand-written body from being read as evidence
	// about the platform.
	AuthoredRoot string `json:"authored_root"`
}

type normCase struct {
	Name     string          `json:"name"`
	Why      string          `json:"why"`
	BodyFile string          `json:"body_file"`
	Stream   bool            `json:"stream"`
	Authored bool            `json:"authored"`
	Expect   json.RawMessage `json:"expect"`
	Error    json.RawMessage `json:"error"`
}

type normExpect struct {
	Text         *string                    `json:"text"`
	ContentKinds *[]string                  `json:"content_kinds"`
	FinishReason *string                    `json:"finish_reason"`
	Model        *string                    `json:"model"`
	Usage        map[string]json.RawMessage `json:"usage"`
}

// assertedCounter reads one counter from an expectation. It returns three outcomes, and keeping
// them apart is the whole point: the key may be absent (the contract asserts nothing about it),
// present and null (the contract asserts it was NOT measured), or present with a number.
func assertedCounter(usage map[string]json.RawMessage, field string) (asserted bool, want *int) {
	raw, present := usage[field]
	if !present {
		return false, nil
	}
	if string(raw) == "null" {
		return true, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return false, nil
	}
	return true, &n
}

func assertedBool(usage map[string]json.RawMessage, field string) (bool, bool) {
	raw, present := usage[field]
	if !present {
		return false, false
	}
	var b bool
	_ = json.Unmarshal(raw, &b)
	return true, b
}

// projection is what this adapter actually produces, expressed in the corpus's observable shape.
// Fields the adapter has no concept of stay nil — that is the difference the report below turns
// into a gap rather than a disagreement.
type projection struct {
	text         string
	contentKinds []string
	finishReason string
	model        string
	input        *int
	output       *int
	cacheRead    *int
	reasoning    *int
	cacheWrite   *int
	estimated    *bool
}

func contractsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "evals", "contracts")
}

// serveBody stands the fixture body up behind a real HTTP server so the adapter's real code path
// runs — decoding included. Feeding the struct in directly would skip the part under test.
func serveBody(t *testing.T, body []byte, stream bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func projectNonStreaming(t *testing.T, body []byte) (projection, error) {
	adapter := &Adapter{BaseURL: serveBody(t, body, false)}
	out, err := adapter.Decide(context.Background(), map[string]any{"model": "m", "messages": []any{}})
	if err != nil {
		return projection{}, err
	}
	p := projection{}
	p.model, _ = out["model"].(string)
	choices, _ := out["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		p.finishReason, _ = choice["finish_reason"].(string)
		msg, _ := choice["message"].(map[string]any)
		p.text, _ = msg["content"].(string)
	}
	if p.text != "" {
		p.contentKinds = []string{"text"}
	} else {
		p.contentKinds = []string{}
	}
	if usage, ok := out["usage"].(map[string]any); ok {
		if v, ok := usage["prompt_tokens"].(int); ok {
			p.input = &v
		}
		if v, ok := usage["completion_tokens"].(int); ok {
			p.output = &v
		}
	}
	return p, nil
}

func projectStreaming(t *testing.T, body []byte) (projection, error) {
	adapter := &Adapter{BaseURL: serveBody(t, body, true)}
	var text strings.Builder
	p := projection{}
	err := adapter.DecideStream(context.Background(), map[string]any{"model": "m", "messages": []any{}}, func(c providers.Chunk) error {
		text.WriteString(c.Delta)
		if c.FinishReason != "" {
			p.finishReason = c.FinishReason
		}
		if c.Model != "" {
			p.model = c.Model
		}
		if c.Usage != nil {
			in, out := c.Usage.PromptTokens, c.Usage.CompletionTokens
			p.input, p.output = &in, &out
			p.cacheRead, p.cacheWrite = c.Usage.CacheReadTokens, c.Usage.CacheWriteTokens
		}
		return nil
	})
	if err != nil {
		return projection{}, err
	}
	p.text = text.String()
	if p.text != "" {
		p.contentKinds = []string{"text"}
	} else {
		p.contentKinds = []string{}
	}
	return p, nil
}

// diffKind separates the two reasons a case can fail, which is the whole point of running a corpus
// we do not yet satisfy. A gap is "this adapter has no concept for what the contract asks"; a
// divergence is "it has the concept and produces something else". Only the second is a bug today —
// the first is FND-004's scope, and knowing exactly which fields it covers is worth more than the
// headline count.
type diffKind int

const (
	diffNone diffKind = iota
	diffGap
	diffDivergence
)

type finding struct {
	field string
	kind  diffKind
	got   string
	want  string
}

func compare(p projection, e normExpect) []finding {
	var out []finding
	add := func(field string, kind diffKind, got, want any) {
		out = append(out, finding{field: field, kind: kind, got: fmt.Sprint(got), want: fmt.Sprint(want)})
	}

	if e.Text != nil && p.text != *e.Text {
		add("text", diffDivergence, p.text, *e.Text)
	}
	if e.ContentKinds != nil && !equalStrings(p.contentKinds, *e.ContentKinds) {
		// A "thinking" part is a concept this adapter does not have at all: it never reads
		// reasoning_content. Anything else here is a real disagreement about text.
		kind := diffDivergence
		if containsStr(*e.ContentKinds, "thinking") || containsStr(*e.ContentKinds, "tool_call") {
			kind = diffGap
		}
		add("content_kinds", kind, p.contentKinds, *e.ContentKinds)
	}
	if e.FinishReason != nil && p.finishReason != *e.FinishReason {
		add("finish_reason", diffDivergence, p.finishReason, *e.FinishReason)
	}
	if e.Model != nil && p.model != *e.Model {
		add("model", diffDivergence, p.model, *e.Model)
	}
	if e.Usage != nil {
		// A case whose expectation says estimated:true wants counters DERIVED from `timings`, which
		// this adapter has no concept of. Missing them there is a gap, not a disagreement.
		_, derived := assertedBool(e.Usage, "estimated")
		cmpCounter := func(field, key string, got *int, haveConcept bool) {
			asserted, want := assertedCounter(e.Usage, key)
			if !asserted {
				return
			}
			switch {
			case want == nil && got == nil:
			case want == nil && got != nil:
				add(field, diffDivergence, *got, "null (not measured)")
			case got == nil:
				k := diffGap
				if haveConcept && !derived {
					k = diffDivergence
				}
				add(field, k, "absent", *want)
			case *got != *want:
				add(field, diffDivergence, *got, *want)
			}
		}
		cmpCounter("usage.input", "input", p.input, true)
		cmpCounter("usage.output", "output", p.output, true)
		cmpCounter("usage.cache_read", "cache_read", p.cacheRead, false)
		cmpCounter("usage.reasoning", "reasoning", p.reasoning, false)
		cmpCounter("usage.cache_write", "cache_write", p.cacheWrite, false)
		if asserted, want := assertedBool(e.Usage, "estimated"); asserted && p.estimated == nil {
			add("usage.estimated", diffGap, "absent", want)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// knownDivergences records the disagreements already understood and scheduled, so the corpus keeps
// teeth for a NEW one without failing the build on work that is already on the roadmap. A divergence
// listed here still prints; what it does not do is hide.
//
// Keeping this list explicit rather than loosening the comparison is deliberate: a silenced
// divergence is indistinguishable from one nobody noticed, which is the failure mode this whole
// corpus exists to prevent.
var knownDivergences = map[string]string{
	"usage.input":  "MDL-014: a provider that reports no usage at all is normalized to 0 rather than 'not measured'",
	"usage.output": "MDL-014: same fabricated zero on the completion counter",
}

// TestNormalizationCorpusAgainstThisAdapter runs the shared normalization corpus against the real
// openai_compatible adapter, at the Synaptum team's explicit request: under H1 = D the equivalence
// between their Python implementation and this Go one is only a design decision until both run the
// same cases.
//
// It does NOT assert that every case passes, and saying so plainly matters: this adapter predates
// the contract and does not implement its shape (no thinking parts, no tool calls, a two-counter
// Usage). Failing the build on that would report a decision already recorded in the roadmap as
// FND-004, not new information.
//
// What it does assert is that nothing DIVERGES: where this adapter has the concept the contract
// asks about, it must produce the contract's answer. A gap is work not done; a divergence is two
// implementations that disagree, which is the failure this corpus exists to catch.
func TestNormalizationCorpusAgainstThisAdapter(t *testing.T) {
	corpusPath := filepath.Join(contractsDir(t), "normalizacion", "fixtures", "openai-compatible.json")
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("reading the shared corpus: %v", err)
	}
	var corpus normCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parsing the shared corpus: %v", err)
	}
	bodyRoot := filepath.Join(filepath.Dir(corpusPath), filepath.FromSlash(corpus.BodyRoot))
	authoredRoot := bodyRoot
	if corpus.AuthoredRoot != "" {
		authoredRoot = filepath.Join(filepath.Dir(corpusPath), filepath.FromSlash(corpus.AuthoredRoot))
	}
	assertBodiesMatchCorpus(t, bodyRoot, corpus.BodyManifestVersion)
	t.Logf("corpus %s %s (%s): %d cases, bodies manifest v%d",
		corpus.Contract, corpus.Version, corpus.Dialect, len(corpus.Cases), corpus.BodyManifestVersion)

	var passed, gapped int
	var divergences []string

	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := bodyRoot
			if tc.Authored {
				root = authoredRoot
			}
			body, err := os.ReadFile(filepath.Join(root, tc.BodyFile))
			if err != nil {
				t.Fatalf("reading body %s: %v", tc.BodyFile, err)
			}

			var p projection
			var runErr error
			if tc.Stream {
				p, runErr = projectStreaming(t, body)
			} else {
				p, runErr = projectNonStreaming(t, body)
			}
			if runErr != nil {
				// The corpus has a case whose body is a mid-stream error; an error here may be the
				// contract's expected outcome rather than a failure.
				if len(tc.Error) > 0 {
					t.Logf("PASS (error case): %v", runErr)
					passed++
					return
				}
				t.Logf("GAP: the adapter returned an error where the contract expects a response: %v", runErr)
				gapped++
				return
			}

			var e normExpect
			if len(tc.Expect) > 0 {
				if err := json.Unmarshal(tc.Expect, &e); err != nil {
					t.Fatalf("parsing expectation: %v", err)
				}
			}

			findings := compare(p, e)
			if len(findings) == 0 {
				t.Logf("PASS")
				passed++
				return
			}
			unexpected := false
			for _, f := range findings {
				label := "GAP"
				if f.kind == diffDivergence {
					label = "DIVERGENCE"
					divergences = append(divergences, fmt.Sprintf("%s → %s: got %q, want %q", tc.Name, f.field, f.got, f.want))
					if reason, known := knownDivergences[f.field]; known {
						label = "DIVERGENCE (conocida — " + reason + ")"
					} else {
						unexpected = true
					}
				}
				t.Logf("%s %-20s got=%q want=%q", label, f.field, f.got, f.want)
			}
			if unexpected {
				t.Errorf("this adapter disagrees with the contract on a concept it does implement, and the disagreement is not a recorded one — see the DIVERGENCE lines above")
				return
			}
			gapped++
		})
	}

	t.Logf("RESUMEN: %d/%d pasan, %d con hueco (FND-004), %d divergencias",
		passed, len(corpus.Cases), gapped, len(divergences))
	for _, d := range divergences {
		t.Logf("DIVERGENCIA: %s", d)
	}
}

// assertBodiesMatchCorpus refuses to run expectations against a body corpus they were not written
// for. Both corpora are vendored copies of files published elsewhere and re-recorded on their own
// schedule, so drifting apart is the normal state, not the exceptional one — and a run against
// mismatched bodies fails in a way that looks like a finding.
func assertBodiesMatchCorpus(t *testing.T, bodyRoot string, want int) {
	t.Helper()
	if want == 0 {
		t.Fatal("the corpus does not declare body_manifest_version — cannot tell which bodies it was written against")
	}
	raw, err := os.ReadFile(filepath.Join(bodyRoot, "..", "manifest.json"))
	if err != nil {
		t.Fatalf("reading the body manifest: %v", err)
	}
	var manifest struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parsing the body manifest: %v", err)
	}
	if manifest.Version != want {
		t.Fatalf("the vendored bodies are manifest v%d but this corpus was written against v%d — re-vendor from the shared folder before reading anything into the result",
			manifest.Version, want)
	}
}
