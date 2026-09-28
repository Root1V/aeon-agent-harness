package tracing

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Attributes the Argus observability platform routes on
// (github.com/Root1V/argus-observability-platform, platform/collector/agent.yaml).
//
// Read from its collector config rather than its README, because the README does not state them and the
// routing connector does. Its condition, verbatim:
//
//	attributes["argus.hot"] == true
//	  or status.code == STATUS_CODE_ERROR
//	  or attributes["argus.guardrail"] != nil
//
// A span matching any of those goes to the HOT path — an alert bus reached in about two seconds, bypassing
// the datastore — as well as the cold one. Everything else waits 30-60s for ClickHouse.
//
// THE PYTHON SIDE USES THEIR SDK (argus-obs-sdk); GO DOES NOT, because they ship none for it. So these
// constants exist to make Go's spans indistinguishable from the ones their SDK produces — the names and the
// VALUE SHAPES below are copied from argus_semconv.guardrails, not invented here.
//
// Three things WERE invented here at first, and all three produce telemetry that looks right and that their
// tooling does not match:
//
//   - `argus.guardrail` is a COMMA-SEPARATED LIST in their model (AgentRun.attributes joins the breaches),
//     so a consumer may split on commas. A single value is a list of one, which is compatible; a value
//     containing a comma would not be, which is why no kind below has one.
//   - Their AgentRun sets `argus.hot` ALONGSIDE the guardrail. The first version here deliberately did not,
//     reasoning that the router triggers on either — true of the router, and wrong for anything downstream
//     filtering on `argus.hot`, which their own spans always carry.
//   - Their kinds are HYPHENATED (`tool-call-budget`, `cost-budget`); ours were snake_case.

const (
	// AttrArgusHot marks a span as urgent when nothing else about it would.
	AttrArgusHot = "argus.hot"
	// AttrArgusGuardrail names the guardrail that refused. NOT a boolean: "denied by policy" and "the run
	// exhausted its budget" both need answering in two seconds and need completely different answers, so
	// the value carries which one it was.
	AttrArgusGuardrail = "argus.guardrail"
)

// Guardrail kinds Aeon reports. These are the platform's own refusals — the things it exists to do — and
// they are exactly what an operator wants woken up for: not "a request failed" but "governance stopped
// something".
const (
	// THEIRS, verbatim from argus_semconv.guardrails, so a breach Aeon reports lands in the same bucket as
	// one their own Budget model reports.
	GuardrailToolCallBudget = "tool-call-budget"
	GuardrailToolCallLoop   = "tool-call-loop"
	GuardrailTokenBudget    = "token-budget"
	GuardrailCostBudget     = "cost-budget"

	// OURS, in their style, for refusals their budget model has no concept of. A Cedar policy denial is not
	// a budget, and reusing `cost-budget` to avoid adding a name would make two unrelated incidents
	// indistinguishable on a dashboard.
	GuardrailPolicyDenied       = "policy-denied"
	GuardrailApprovalRequired   = "approval-required"
	GuardrailFanOutExceeded     = "fan-out-budget"
	GuardrailDestinationUnknown = "destination-not-declared"
)

// There are deliberately NO constants for the modality refusal (MDL-011) or the circuit breaker (A5).
//
// Both are real guardrails and NEITHER of their handlers creates a span, so a constant for them is a name
// with nowhere to be set. The previous commit's roadmap entry CLAIMED they had been left out and they were
// still here, unused — the script that removed them failed its own assertion and I did not check. A
// document asserting something the code does not do, which is the defect this project has spent a week
// finding in other people's artefacts and has now produced in its own.

// MarkGuardrail records that a guardrail refused this operation.
//
// It does NOT set the span status to Error, and that distinction is deliberate: a policy denial is the
// system working correctly, and reporting it as an error would bury real failures under a stream of
// successful refusals. Argus routes it to the hot path either way — the guardrail attribute is its own
// trigger, which is why the attribute exists separately from the error status.
func MarkGuardrail(span trace.Span, kind, reason string) {
	if span == nil {
		return
	}
	span.SetAttributes(
		attribute.String(AttrArgusGuardrail, kind),
		// BOTH, because their AgentRun.attributes() emits both. The router triggers on either, so the first
		// version here set only the guardrail — which left every Aeon span invisible to anything downstream
		// filtering on argus.hot, while their own spans always carry it.
		attribute.Bool(AttrArgusHot, true),
		attribute.String("aeon.guardrail.reason", reason),
	)
}

// MarkHot marks a span urgent for a reason that is neither an error nor a guardrail.
//
// Unused by the gateways, and kept for one reason: MarkGuardrail now sets argus.hot itself, so the only
// caller left would be something urgent that is NOT a guardrail breach — and reaching for a bare "this is
// important" flag is how a hot path fills up until nobody reads it.
func MarkHot(span trace.Span) {
	if span == nil {
		return
	}
	span.SetAttributes(attribute.Bool(AttrArgusHot, true))
}
