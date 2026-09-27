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
	UsageState   map[string]string          `json:"usage_state"`
	// UsageRelations are triples like ["input", ">=", "cache_read"]: assertions about the RELATIONSHIP
	// between counters rather than their values. They are how the contract states "input is inclusive of
	// cache" without pinning numbers that change every time the bodies are re-recorded.
	UsageRelations [][]string `json:"usage_relations"`
	ToolCalls      []struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"tool_calls"`
	ToolCallIDsAreNonEmpty *bool     `json:"tool_call_ids_are_nonempty"`
	EventKinds             *[]string `json:"event_kinds"`
	// EventKindsCollapsed is EventKinds with runs of the same kind squashed to one, so a case can assert
	// the LIFECYCLE without depending on how many delta events a particular recording happens to contain.
	EventKindsCollapsed  *[]string `json:"event_kinds_collapsed"`
	StopsAtFirstSentinel *bool     `json:"stops_at_first_sentinel"`
	PartialText          *string   `json:"partial_text"`
}

// implementedExpectKeys is every expectation key this runner actually READS.
//
// THE GUARD THAT MATTERS, and it exists because of what it found. This runner's expectation struct
// covered five keys while the contract used thirteen, so the other eight were silently ignored — and
// three cases had NO key the runner read, meaning they passed without a single assertion being checked.
// Two of those three were in the 5/14 figure published to the other two teams in A-41 and A-42. A
// number computed by not looking is worse than no number: it was reported as evidence.
//
// So an expectation key this runner does not implement is now a FAILURE, not silence. The same shape as
// roadmap_check refusing a row it cannot parse instead of skipping it: a checker that quietly ignores
// what it does not understand grows blind spots exactly where the contract grows.
var implementedExpectKeys = map[string]bool{
	"text": true, "content_kinds": true, "finish_reason": true, "model": true,
	"usage": true, "usage_state": true, "usage_relations": true,
	"tool_calls": true, "tool_call_ids_are_nonempty": true,
	"event_kinds": true, "event_kinds_collapsed": true,
	"stops_at_first_sentinel": true, "partial_text": true,
}

// assertEveryExpectKeyIsImplemented fails on any key the runner would otherwise ignore.
func assertEveryExpectKeyIsImplemented(t *testing.T, caseName string, rawExpect json.RawMessage) {
	t.Helper()
	if len(rawExpect) == 0 {
		return
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(rawExpect, &keys); err != nil {
		t.Fatalf("case %q: expectation is not an object: %v", caseName, err)
	}
	for k := range keys {
		if !implementedExpectKeys[k] {
			t.Errorf("case %q asserts %q and this runner does not read it — the case would PASS without that "+
				"assertion ever being checked, which is how 5/14 got published as a result", caseName, k)
		}
	}
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
	toolCalls    []providers.ToolCall
	// eventKinds is the streaming lifecycle observed, in order. Recorded so a case can assert that the
	// reasoning phase CLOSES before the tool call opens, which no amount of inspecting the final message
	// can show.
	eventKinds  []string
	partialText string
	// sentinelStops records that the adapter stopped at the first [DONE] rather than continuing to read.
	sentinelStops bool
}

// counter reads a projected counter by the contract's field name, so the comparison code names fields
// once instead of repeating a switch per assertion kind.
func (p projection) counter(field string) (*int, bool) {
	switch field {
	case "input":
		return p.input, true
	case "output":
		return p.output, true
	case "cache_read":
		return p.cacheRead, true
	case "reasoning":
		return p.reasoning, true
	case "cache_write":
		return p.cacheWrite, true
	default:
		return nil, false
	}
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
	p := projection{contentKinds: []string{}}
	p.model, _ = out["model"].(string)
	choices, _ := out["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		p.finishReason, _ = choice["finish_reason"].(string)
		msg, _ := choice["message"].(map[string]any)
		p.text, _ = msg["content"].(string)

		// The typed parts, read from the response rather than inferred from whether text is empty. The old
		// projection derived content_kinds from `p.text != ""`, which is why every thinking and tool_call
		// expectation could only ever be reported as a gap: the runner had no way to see them even once the
		// adapter produced them.
		if parts, ok := msg["content_parts"].([]providers.ContentPart); ok {
			for _, part := range parts {
				p.contentKinds = append(p.contentKinds, part.Kind)
				if part.Kind == providers.PartToolCall && part.ToolCall != nil {
					p.toolCalls = append(p.toolCalls, *part.ToolCall)
				}
			}
		}
	}
	if usage, ok := out["usage"].(map[string]any); ok {
		readCounter := func(key string) *int {
			if v, ok := usage[key].(int); ok {
				return &v
			}
			return nil
		}
		p.input, p.output = readCounter("prompt_tokens"), readCounter("completion_tokens")
		p.cacheRead, p.cacheWrite = readCounter("cache_read_tokens"), readCounter("cache_write_tokens")
		p.reasoning = readCounter("reasoning_tokens")
		if v, ok := usage["estimated"].(bool); ok {
			p.estimated = &v
		}
	}
	return p, nil
}

func projectStreaming(t *testing.T, body []byte) (projection, error) {
	adapter := &Adapter{BaseURL: serveBody(t, body, true)}
	var text, reasoning strings.Builder
	p := projection{contentKinds: []string{}}
	// sawToolCallEnd/etc. are tracked so content_kinds can be derived from the LIFECYCLE rather than from
	// whether the accumulated text happens to be empty — the old projection did the latter, which is why
	// no thinking or tool_call expectation could ever be observed even once the adapter produced them.
	sawThinking, sawText := false, false

	err := adapter.DecideStream(context.Background(), map[string]any{"model": "m", "messages": []any{}}, func(c providers.Chunk) error {
		if c.Event != "" {
			p.eventKinds = append(p.eventKinds, string(c.Event))
		}
		text.WriteString(c.Delta)
		reasoning.WriteString(c.ReasoningDelta)
		if c.ReasoningDelta != "" {
			sawThinking = true
		}
		if c.Delta != "" {
			sawText = true
		}
		if c.ToolCall != nil {
			p.toolCalls = append(p.toolCalls, *c.ToolCall)
		}
		if c.FinishReason != "" {
			p.finishReason = c.FinishReason
		}
		if c.Model != "" {
			p.model = c.Model
		}
		if c.Usage != nil {
			// Straight through since MDL-014: these are pointers at the wire now, so a counter the
			// upstream omitted stays nil instead of being reborn as a zero on the way into the
			// projection — which is exactly the divergence this corpus reported for ten cases.
			p.input, p.output = c.Usage.PromptTokens, c.Usage.CompletionTokens
			p.cacheRead, p.cacheWrite = c.Usage.CacheReadTokens, c.Usage.CacheWriteTokens
			p.reasoning = c.Usage.ReasoningTokens
			p.estimated = c.Usage.Estimated
		}
		return nil
	})
	// partialText is recorded WHETHER OR NOT the stream failed, because the contract's assertion is about
	// an interrupted stream: the deltas already emitted are not retracted, since they were generated and
	// paid for. Reading it only on success would leave that property unverifiable.
	p.partialText = text.String()
	if err != nil {
		return projection{partialText: text.String(), eventKinds: p.eventKinds}, err
	}
	p.text = text.String()
	if sawThinking {
		p.contentKinds = append(p.contentKinds, providers.PartThinking)
	}
	if sawText {
		p.contentKinds = append(p.contentKinds, providers.PartText)
	}
	for range p.toolCalls {
		p.contentKinds = append(p.contentKinds, providers.PartToolCall)
	}
	// stops_at_first_sentinel, computed from THIS body rather than assumed.
	//
	// The recording for that case carries exactly one [DONE] with nothing after it, so the property is only
	// weakly observable here: there is nothing the adapter could have wrongly read. Rather than hard-code
	// `true` — which would report an assertion as checked when nothing was checked, the exact failure the
	// unknown-key guard above exists to prevent — this looks at whatever follows the first sentinel in the
	// body and verifies none of it surfaced. It answers truthfully for this recording and gets stronger for
	// free if the body ever gains a trailing chunk.
	//
	// The property is proved properly in TestStreamStopsAtFirstSentinel, with a body built to trap it.
	p.sentinelStops = !leakedPastFirstSentinel(body, p.text)
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
		// FND-004 CLOSED THIS GAP, so a mismatch here is now a DIVERGENCE. The adapter reads
		// reasoning_content and tool_calls and emits typed parts, so "we have no concept for this" has
		// stopped being true — and leaving the gap classification in place would have let a real
		// disagreement keep reporting itself as work not done.
		add("content_kinds", diffDivergence, p.contentKinds, *e.ContentKinds)
	}
	if e.FinishReason != nil && p.finishReason != *e.FinishReason {
		add("finish_reason", diffDivergence, p.finishReason, *e.FinishReason)
	}
	if e.Model != nil && p.model != *e.Model {
		add("model", diffDivergence, p.model, *e.Model)
	}
	if e.Usage != nil {
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
				add(field, diffDivergence, "absent", *want)
			case *got != *want:
				add(field, diffDivergence, *got, *want)
			}
		}
		cmpCounter("usage.input", "input", p.input, true)
		cmpCounter("usage.output", "output", p.output, true)
		cmpCounter("usage.cache_read", "cache_read", p.cacheRead, false)
		cmpCounter("usage.reasoning", "reasoning", p.reasoning, false)
		cmpCounter("usage.cache_write", "cache_write", p.cacheWrite, false)
		if asserted, want := assertedBool(e.Usage, "estimated"); asserted {
			switch {
			case p.estimated == nil:
				add("usage.estimated", diffDivergence, "absent", want)
			case *p.estimated != want:
				add("usage.estimated", diffDivergence, *p.estimated, want)
			}
		}
	}

	// usage_state asserts the THREE-STATE of each counter without pinning a number, which is what lets the
	// bodies be re-recorded without rewriting the corpus. "measured" says a value arrived; "unmeasured"
	// says the key is absent. A state name this runner does not know is a failure rather than a skip — the
	// same rule as an unknown expectation key.
	for field, want := range e.UsageState {
		got, known := p.counter(field)
		if !known {
			add("usage_state."+field, diffDivergence, "unknown counter", want)
			continue
		}
		switch want {
		case "measured":
			if got == nil {
				add("usage_state."+field, diffDivergence, "unmeasured", "measured")
			}
		case "unmeasured":
			if got != nil {
				add("usage_state."+field, diffDivergence, fmt.Sprintf("measured (%d)", *got), "unmeasured")
			}
		default:
			add("usage_state."+field, diffDivergence, "this runner does not implement state "+want, want)
		}
	}

	// usage_relations is how the contract states "input is INCLUSIVE of cache" without depending on the
	// numbers in any particular recording. It is the only assertion here that would survive a re-record
	// unchanged, which is exactly why it is the one worth having.
	for _, rel := range e.UsageRelations {
		if len(rel) != 3 {
			add("usage_relations", diffDivergence, fmt.Sprintf("malformed relation %v", rel), "a triple")
			continue
		}
		left, op, right := rel[0], rel[1], rel[2]
		l, lk := p.counter(left)
		r, rk := p.counter(right)
		if !lk || !rk {
			add("usage_relations", diffDivergence, fmt.Sprintf("unknown counter in %v", rel), "known counters")
			continue
		}
		if l == nil || r == nil {
			add("usage_relations", diffDivergence,
				fmt.Sprintf("%s or %s is unmeasured, so %v cannot be checked", left, right, rel), fmt.Sprint(rel))
			continue
		}
		ok := false
		switch op {
		case ">=":
			ok = *l >= *r
		case ">":
			ok = *l > *r
		case "==":
			ok = *l == *r
		default:
			add("usage_relations", diffDivergence, "this runner does not implement operator "+op, op)
			continue
		}
		if !ok {
			add("usage_relations", diffDivergence, fmt.Sprintf("%s=%d %s %s=%d is false", left, *l, op, right, *r), fmt.Sprint(rel))
		}
	}

	// tool_calls: name and DECODED arguments. Comparing the decoded map is the assertion that matters —
	// a runner comparing the raw string would pass an adapter that never parsed it, which is the very
	// thing the contract requires.
	if e.ToolCalls != nil {
		if len(p.toolCalls) != len(e.ToolCalls) {
			add("tool_calls", diffDivergence, fmt.Sprintf("%d call(s)", len(p.toolCalls)), fmt.Sprintf("%d call(s)", len(e.ToolCalls)))
		} else {
			for i, want := range e.ToolCalls {
				got := p.toolCalls[i]
				if got.Name != want.Name {
					add(fmt.Sprintf("tool_calls[%d].name", i), diffDivergence, got.Name, want.Name)
				}
				if !sameArgs(got.Arguments, want.Arguments) {
					add(fmt.Sprintf("tool_calls[%d].arguments", i), diffDivergence, got.Arguments, want.Arguments)
				}
			}
		}
	}
	if e.ToolCallIDsAreNonEmpty != nil && *e.ToolCallIDsAreNonEmpty {
		if len(p.toolCalls) == 0 {
			add("tool_call_ids_are_nonempty", diffDivergence, "no tool calls at all", "ids on every call")
		}
		for i, tc := range p.toolCalls {
			if tc.ID == "" {
				add(fmt.Sprintf("tool_calls[%d].id", i), diffDivergence, "empty", "non-empty")
			}
		}
	}

	if e.EventKinds != nil && !equalStrings(p.eventKinds, *e.EventKinds) {
		add("event_kinds", diffDivergence, p.eventKinds, *e.EventKinds)
	}
	if e.EventKindsCollapsed != nil {
		if got := collapseRuns(p.eventKinds); !equalStrings(got, *e.EventKindsCollapsed) {
			add("event_kinds_collapsed", diffDivergence, got, *e.EventKindsCollapsed)
		}
	}
	if e.StopsAtFirstSentinel != nil && *e.StopsAtFirstSentinel && !p.sentinelStops {
		add("stops_at_first_sentinel", diffDivergence, false, true)
	}
	if e.PartialText != nil && p.partialText != *e.PartialText {
		add("partial_text", diffDivergence, p.partialText, *e.PartialText)
	}
	return out
}

// sameArgs compares decoded argument maps through their canonical JSON.
//
// Through JSON rather than reflect.DeepEqual because the two sides decode from different places: the
// expectation's numbers arrive as float64 from the corpus and the adapter's from the body, and DeepEqual
// would report a difference between two values that are the same number.
func sameArgs(got, want map[string]any) bool {
	a, err1 := json.Marshal(got)
	b, err2 := json.Marshal(want)
	return err1 == nil && err2 == nil && string(a) == string(b)
}

// collapseRuns squashes consecutive identical kinds to one, so a case can assert the LIFECYCLE without
// depending on how many delta events a particular recording happens to carry.
func collapseRuns(in []string) []string {
	out := make([]string, 0, len(in))
	for i, k := range in {
		if i == 0 || in[i-1] != k {
			out = append(out, k)
		}
	}
	return out
}

// equalStrings compares two sequences IN ORDER.
//
// It used to sort both sides first, and that was wrong in a way only a mutation found. The contract's
// lists are ordered: content_kinds is ["thinking", "text"] because reasoning precedes the answer, and
// event_kinds is a LIFECYCLE where the order is the entire property. The case named "the reasoning phase
// closes when the tool call starts, not when the stream ends" passed with the closing removed, because
// both orderings contain the same multiset of events.
//
// So a sorted comparison could not fail the one assertion it existed to make. Order-sensitive now.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
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
// Empty since MDL-014 fixed the last two entries, and empty is the point: this map is a list of
// disagreements we have DECIDED to live with, so a fixed one must leave it or it becomes a graveyard of
// stale excuses that quietly forgive a real regression.
//
// verifyKnownDivergencesStillDiverge below is what keeps that true without anyone remembering.
var knownDivergences = map[string]string{}

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

	// pendingKnown starts as a copy of knownDivergences and loses an entry each time that divergence is
	// actually observed. Whatever is left at the end is an excuse for a disagreement that no longer
	// happens — checked below, because a list of accepted divergences that nobody prunes stops being a
	// record of decisions and becomes a blanket that forgives the next real regression.
	pendingKnown := make(map[string]string, len(knownDivergences))
	for field, reason := range knownDivergences {
		pendingKnown[field] = reason
	}
	seenDivergences := map[string]bool{}

	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			// Before anything else: refuse to evaluate a case whose assertions this runner cannot read.
			assertEveryExpectKeyIsImplemented(t, tc.Name, tc.Expect)

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
					// The error was expected, but the case may ALSO assert what survived it — and it does:
					// partial_text. Returning here used to count the case as passing on the error alone,
					// without ever checking that the already-emitted deltas were preserved, which is the
					// actual property. So the expectation is still compared.
					var ee normExpect
					if len(tc.Expect) > 0 {
						if err := json.Unmarshal(tc.Expect, &ee); err != nil {
							t.Fatalf("parsing expectation: %v", err)
						}
					}
					if findings := compare(p, ee); len(findings) > 0 {
						for _, f := range findings {
							t.Errorf("error case %s: got=%q want=%q", f.field, f.got, f.want)
						}
						return
					}
					t.Logf("PASS (error case, and what it preserved was checked): %v", runErr)
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
						seenDivergences[f.field] = true
					} else {
						unexpected = true
					}
				}
				t.Logf("%s %-20s got=%q want=%q", label, f.field, f.got, f.want)
			}
			for field := range seenDivergences {
				delete(pendingKnown, field)
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

	// Whatever is left in pendingKnown is an accepted divergence that no longer happens. Removing it is
	// not tidiness: a stale exception is indistinguishable from a live one, so it would forgive the next
	// real regression on that field in silence. Enforcing it here means nobody has to remember.
	for field, reason := range pendingKnown {
		t.Errorf("knownDivergences still lists %q (%q) but this adapter no longer diverges there — remove the entry", field, reason)
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

// leakedPastFirstSentinel reports whether any text appearing AFTER the first [DONE] ended up in the output.
func leakedPastFirstSentinel(body []byte, gotText string) bool {
	idx := strings.Index(string(body), sseDataPrefix+sseDone)
	if idx < 0 {
		return false
	}
	tail := string(body)[idx+len(sseDataPrefix+sseDone):]
	for _, line := range strings.Split(tail, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, sseDataPrefix) {
			continue
		}
		var parsed streamChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, sseDataPrefix)), &parsed); err != nil {
			continue
		}
		for _, ch := range parsed.Choices {
			if ch.Delta.Content != "" && strings.Contains(gotText, ch.Delta.Content) {
				return true
			}
		}
	}
	return false
}

// TestStreamStopsAtFirstSentinel proves the property the corpus body cannot.
//
// The shared contract requires exactly one [DONE] per response, so a second one is the upstream
// misbehaving — and the honest question is what WE do when it happens, not whether it happens. The body
// here carries a second sentinel followed by a chunk of text, so an adapter that kept parsing past the
// first would surface it. This lives outside the corpus because it needs a body the corpus does not (and
// should not) contain: a recording of correct behaviour cannot demonstrate resilience to incorrect input.
func TestStreamStopsAtFirstSentinel(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"LEAKED-PAST-THE-SENTINEL\"}}]}\n\n" +
		"data: [DONE]\n\n")

	p, err := projectStreaming(t, body)
	if err != nil {
		t.Fatalf("streaming: %v", err)
	}
	if strings.Contains(p.text, "LEAKED") {
		t.Errorf("text = %q — the adapter kept reading past the first [DONE]", p.text)
	}
	if p.text != "Hi" {
		t.Errorf("text = %q, want %q", p.text, "Hi")
	}
	if !p.sentinelStops {
		t.Error("sentinelStops is false on a body built to trap exactly this — the projection is not observing what it claims")
	}
}

// TestToolArgumentsThatDoNotParseAreAnError covers a contract rule the CORPUS DOES NOT COVER.
//
// The spec is explicit: «Una cadena que no parsea no se convierte en objeto vacío. Es un error del
// proveedor y se levanta como tal — un `{}` silencioso ejecutaría la herramienta sin argumentos.» Every
// recorded body has well-formed arguments, so nothing in the shared corpus exercises it — and a mutation
// that deleted the error check left all 14 cases green.
//
// That is the honest limit of a corpus built from recordings: it proves agreement on what the platform
// actually sends, and says nothing about what happens when the platform misbehaves. This test is the other
// half, and it belongs here rather than in the shared fixtures, which should keep describing reality.
func TestToolArgumentsThatDoNotParseAreAnError(t *testing.T) {
	t.Run("non-streaming", func(t *testing.T) {
		body := []byte(`{"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant",` +
			`"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{not json"}}]}}]}`)
		adapter := &Adapter{BaseURL: serveBody(t, body, false)}
		out, err := adapter.Decide(context.Background(), map[string]any{"model": "m", "messages": []any{}})
		if err == nil {
			t.Fatalf("unparseable arguments produced a response instead of an error: %v — the tool would run with no arguments and the call would look successful", out)
		}
		if !strings.Contains(err.Error(), "get_weather") {
			t.Errorf("error = %v, want it to name the tool whose arguments could not be read", err)
		}
	})

	t.Run("streaming", func(t *testing.T) {
		// The streaming path parses ONCE, at the end, from the accumulated fragments — so this also pins
		// that the single parse is checked. A version that parsed per fragment would fail on the happy path
		// instead, which is the failure the contract warns about.
		body := []byte(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"get_weather","arguments":"{not"}}]}}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" json"}}]}}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
			"data: [DONE]\n\n")
		if _, err := projectStreaming(t, body); err == nil {
			t.Fatal("unparseable streamed arguments produced no error")
		}
	})

	t.Run("a tool with no arguments at all IS an empty object", func(t *testing.T) {
		// The one case where `{}` is right, and it has to stay working: a tool with no parameters genuinely
		// sends nothing, and treating that as malformed would refuse a legitimate call.
		args, err := providers.DecodeToolArguments("")
		if err != nil {
			t.Fatalf("empty arguments: %v", err)
		}
		if len(args) != 0 {
			t.Errorf("args = %v, want an empty object", args)
		}
	})
}
