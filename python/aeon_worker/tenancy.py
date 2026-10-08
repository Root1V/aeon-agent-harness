"""The two strings that have to mean the same thing in Go and in Python (VRT-AEON-005).

WHY THEY LIVE TOGETHER IN A MODULE OF THEIR OWN. Both are contracts with the Go side and neither has
a schema: a memo key and a header name are plain strings, so nothing refuses a mismatch — a
misspelling makes the workflow read an empty tenant, or makes the gateway ignore the header, and in
both cases the system keeps working while falling back to the worker's own tenant. That is not a
visible failure, it is the VRT-AEON-005 defect restored by a typo.

So they are in one file, with the Go symbol named beside each, and
`go/internal/api/cross_language_tenancy_test.go` reads THIS FILE and fails if either drifts. Same
reasoning as the golden-corpus drift test: a shared constant with no shared definition needs
something that actually compares the two.
"""
from __future__ import annotations

# Set by the Run Controller at start, from the authenticated caller's tenant.
# Must equal go/internal/runcontroller.TenantMemoKey.
TENANT_MEMO_KEY = "aeon_tenant"

# Sent by the worker on every governed call a run's step makes.
# Must equal go/internal/api.RunTenantHeader.
RUN_TENANT_HEADER = "X-Aeon-Run-Tenant"
