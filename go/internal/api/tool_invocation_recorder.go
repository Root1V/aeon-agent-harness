package api

import (
	"context"

	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// ToolInvocationRecorder adapts the bitácora store to what the executor records into (INT-013/014).
//
// ONE DEFINITION, AND THAT IS THE POINT. This translation is a field-by-field copy between two
// structs that are deliberately separate — `toolexec` must not import the store, or a tool executor
// would need Postgres to be constructible — and it was written twice: once in cmd/aeon-toolgw and
// once in the acceptance test. INT-014 added `disposition` and `policy_id`, both copies dropped
// them, and the only reason it was not shipped is that migration 0005's CHECK refuses a denial with
// no disposition: the database caught a row that would have lied. A hand-written copy in N places
// is N chances to drop a field in silence, so there is now one place.
//
// defaultTenant is WHERE A ROW IS FILED when the invocation has no tenant, not an authorization. An
// MCP call executes with an empty tenant on purpose (GOV-001g: tenant-scoped tools must refuse it
// rather than fall back to a deployment-wide store), and that already happened by the time this
// runs. Filing under the deployment's tenant is what makes the row readable by the operator who
// runs the gateway; leaving it empty would write rows no tenant can query. The row still carries
// its door and an empty run_id, so nothing about the call is misrepresented by where it is filed.
func ToolInvocationRecorder(s *store.Store, defaultTenant string) toolexec.Recorder {
	return func(ctx context.Context, rec toolexec.InvocationRecord) error {
		filedUnder := rec.Tenant
		if filedUnder == "" {
			filedUnder = defaultTenant
		}
		return s.ToolInvocationsFor(filedUnder).Record(ctx, store.ToolInvocation{
			ToolName:         rec.ToolName,
			Door:             rec.Door,
			Outcome:          rec.Outcome,
			ErrorMessage:     rec.ErrorMessage,
			RunID:            rec.RunID,
			StepID:           rec.StepID,
			AgentManifestRef: rec.AgentManifestRef,
			DurationMS:       rec.DurationMS,
			Disposition:      rec.Disposition,
			PolicyID:         rec.PolicyID,
		})
	}
}
