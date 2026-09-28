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
// WHY THIS IS CONFIGURATION AND NOT A DEPENDENCY. Argus ships argus-sdk and argus-semconv, and Aeon uses
// neither: the platform is OTel-native and ingests plain OTLP on 4317/4318, so the integration is two
// attribute names and an endpoint. That keeps a deployment without Argus fully traced, and it is the same
// rule this codebase applies to every provider — nothing in Aeon should require somebody's SDK to work.
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
	GuardrailPolicyDenied       = "policy_denied"
	GuardrailBudgetExhausted    = "budget_exhausted"
	GuardrailApprovalRequired   = "approval_required"
	GuardrailFanOutExceeded     = "fan_out_exceeded"
	GuardrailDestinationUnknown = "destination_not_declared"
	GuardrailModalityRefused    = "modality_refused"
	GuardrailCircuitOpen        = "circuit_open"
)

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
		attribute.String("aeon.guardrail.reason", reason),
	)
}

// MarkHot marks a span urgent for a reason that is neither an error nor a guardrail.
//
// Kept deliberately unused by the gateways today. The two triggers above already cover every case Aeon
// produces, and reaching for a manual "this is important" flag is how a hot path fills up until nobody
// reads it. It exists so that a future caller has the named constant rather than a string literal.
func MarkHot(span trace.Span) {
	if span == nil {
		return
	}
	span.SetAttributes(attribute.Bool(AttrArgusHot, true))
}
