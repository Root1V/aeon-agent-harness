package api

import (
	"net/http"
	"regexp"

	"github.com/aeon-ai/aeon/go/internal/auth"
)

// RunTenantHeader carries the tenant of the RUN a request is a step of.
//
// WHY THIS EXISTS (VRT-AEON-005, Veritium's réplica of 2026-10-08, measured against d73da2b on their
// own deployment): every surface in this package derived the tenant from the caller, and for a step
// of a run the caller is the AEON WORKER — one token, therefore one tenant. So in a shared
// deployment with one worker, every run was judged and billed against the worker's tenant whoever
// submitted it. They measured the policy bundle; verifying it turned up five more surfaces, and the
// worst of them has no symptom at all: the cost ceiling reads the caller's agent registry, finds no
// manifest for the run's agent, LOGS IT AND LETS THE CALL THROUGH (MDL-017's deliberate choice, so
// as not to break deployments whose agents are not registered). Measured: a 1-token ceiling in
// tenant-b with the caller in tenant-ops answers 200.
//
// A HEADER AND NOT A BODY FIELD, and the difference from the defect this codebase already paid for
// twice is the whole design. The Memory Store used to read `tenant_id` from every request body, so
// the isolation SEC-004 built was real and the tenant was the caller's choice; SEC-005 had the same
// shape with the Cedar principal. `rejectRequestTenant` still refuses a body field FROM EVERYONE and
// is untouched. This header is refused too — unless the operator has explicitly listed the tenant on
// the caller (auth.Caller.MayActForTenants, no wildcard). The tenant is still operator-assigned;
// what is new is that an operator can say "this process executes runs for these tenants", which is a
// sentence only an operator can write.
const RunTenantHeader = "X-Aeon-Run-Tenant"

// runTenantPattern mirrors auth's own, so a header that cannot be a tenant is a 400 here rather than
// a predicate or a Cedar principal somewhere further in.
var runTenantPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// effectiveTenant is the tenant a request's reads and writes belong to: the run's when the caller is
// entitled to name one, and otherwise the caller's own — which is every request that existed before
// this header did.
//
// THREE OUTCOMES AND NOT TWO, which is the shape this codebase keeps arriving at:
//
//	header absent          -> the caller's tenant. Unchanged behaviour, so a single-tenant
//	                          deployment needs no configuration and cannot be broken by this.
//	header == own tenant   -> the caller's tenant, no privilege needed. A worker in a one-tenant
//	                          deployment names its own tenant and is simply right.
//	header, not entitled   -> 403, naming the privilege that is missing.
//
// 403 AND NOT 404, deliberately, against T-7's rule that a cross-tenant answer must be
// indistinguishable from a nonexistent one. T-7 is about whether a RESOURCE exists, and a 403 there
// confirms it does. Nothing is being looked up here: the caller is naming a tenant it has no right
// to name, which is the same fact as `ActsAs`'s 403 — "you may not do this" — and hiding it would
// leave an operator debugging a worker whose header is ignored for no stated reason.
func effectiveTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		// No caller means this route was mounted without auth.Require. Serving it anyway is how an
		// unauthenticated surface comes back one wiring mistake at a time.
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: this route was reached without an authenticated caller (SEC-005)",
		})
		return "", false
	}

	requested := r.Header.Get(RunTenantHeader)
	if requested == "" {
		return caller.Tenant, true
	}
	if !runTenantPattern.MatchString(requested) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": RunTenantHeader + " must match " + runTenantPattern.String() +
				" — a plain identifier, so it can be a safe part of a database predicate and of a Cedar " +
				"principal without quoting rules",
		})
		return "", false
	}
	if !caller.ActsForTenant(requested) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "caller " + caller.ID + " may not act for tenant " + requested + ". A caller may name " +
				"another tenant only when the operator has listed it in mayActForTenants, and there is no " +
				"wildcard: a credential that may speak for every tenant is the isolation boundary removed " +
				"while the bundle still looks configured",
			"caller_tenant": caller.Tenant,
		})
		return "", false
	}
	return requested, true
}

// effectiveTenantOrCaller is effectiveTenant for the places that already decided how to report a
// failure, or that must not fail: it returns the caller's tenant when the header is absent or not
// permitted, and never writes a response.
//
// USED ONLY WHERE A REFUSAL WOULD BE WORSE THAN A WRONG TENANT, which today is the journals that
// record a denial. journalDenial must never fail its request — a denial whose record could not be
// written is still a denial, and turning it into a 500 would let a journal outage get an effect
// executed on retry. So it takes the best tenant available and the route that decides the request
// has already refused an unentitled header anyway.
func effectiveTenantOrCaller(r *http.Request) string {
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		return ""
	}
	requested := r.Header.Get(RunTenantHeader)
	if requested != "" && runTenantPattern.MatchString(requested) && caller.ActsForTenant(requested) {
		return requested
	}
	return caller.Tenant
}
