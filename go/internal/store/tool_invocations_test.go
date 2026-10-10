package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestTheBitacoraRefusesARowThatLies is the control on migration 0004's CHECK constraints rather
// than on Record, which cannot produce such a row: Record derives the message from the outcome.
// The constraint exists for the NEXT writer — a second code path, a backfill, a repair script — and
// a constraint nobody exercises is indistinguishable from one that was never applied.
func TestTheBitacoraRefusesARowThatLies(t *testing.T) {
	dsn := os.Getenv("AEON_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("AEON_TEST_PG_DSN not set — skipping Postgres integration test (see make test-go-integration)")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	insert := func(outcome string, errMessage *string) error {
		_, err := conn.Exec(ctx,
			`INSERT INTO tool_invocations (tenant_id, tool_name, door, outcome, error_message)
			 VALUES ('int-013-check', 'probe', 'http', $1, $2)`, outcome, errMessage)
		return err
	}
	insertWithDisposition := func(outcome, disposition string) error {
		_, err := conn.Exec(ctx,
			`INSERT INTO tool_invocations (tenant_id, tool_name, door, outcome, disposition)
			 VALUES ('int-013-check', 'probe', 'http', $1, $2)`, outcome, disposition)
		return err
	}
	msg := "something went wrong"

	t.Run("a success carrying an error message is refused", func(t *testing.T) {
		if err := insert(ToolInvocationOK, &msg); err == nil {
			t.Fatal("the row was accepted: a successful invocation with an error message is a row that lies to an audit")
		} else if !strings.Contains(err.Error(), "tool_invocations_error_message_iff_error") {
			t.Errorf("refused, but not by the constraint that should refuse it: %v", err)
		}
	})

	t.Run("a failure with no message is refused", func(t *testing.T) {
		if err := insert(ToolInvocationError, nil); err == nil {
			t.Fatal("the row was accepted: a failed invocation must carry why, or the record is a shrug")
		}
	})

	// 'denied' USED TO BE THE CASE HERE, and when INT-014 made it a legal outcome this subtest kept
	// passing — on the disposition constraint, not on the one its name claims. A test that survives
	// the thing it names becoming legal is naming something else, so the undefined outcome is now
	// one that is genuinely undefined.
	t.Run("an outcome nobody defined is refused", func(t *testing.T) {
		if err := insert("maybe", nil); err == nil {
			t.Fatal(`'maybe' was accepted: an outcome outside the four the gateway can observe would ` +
				`put rows in the bitácora that no reader can interpret`)
		} else if !strings.Contains(err.Error(), "tool_invocations_outcome_valid") {
			t.Errorf("refused, but not by the outcome constraint: %v", err)
		}
	})

	// INT-014's own constraint: the disposition is what makes a denial actionable (INT-010), and it
	// is required on a denial and forbidden anywhere else.
	t.Run("a denial with no disposition is refused", func(t *testing.T) {
		if err := insertWithDisposition(ToolInvocationDenied, ""); err == nil {
			t.Fatal(`a denial with no disposition was accepted: "refused" and "refused, but a person ` +
				`could approve this" are different facts, and a row that states neither cannot say why a run stopped`)
		} else if !strings.Contains(err.Error(), "tool_invocations_disposition_iff_denied") {
			t.Errorf("refused, but not by the disposition constraint: %v", err)
		}
	})

	t.Run("a disposition on something that was not denied is refused", func(t *testing.T) {
		if err := insertWithDisposition(ToolInvocationOK, "deny_step"); err == nil {
			t.Fatal("a successful invocation carrying a denial disposition was accepted")
		}
	})

	t.Run("a denial WITH its disposition is accepted, and a replay with none", func(t *testing.T) {
		if err := insertWithDisposition(ToolInvocationDenied, "deny_step"); err != nil {
			t.Fatalf("a denial with its disposition was refused: %v", err)
		}
		if err := insertWithDisposition(ToolInvocationReplayed, ""); err != nil {
			t.Fatalf("a replay was refused: %v", err)
		}
	})

	t.Run("the honest pair is accepted", func(t *testing.T) {
		if err := insert(ToolInvocationError, &msg); err != nil {
			t.Fatalf("a failure WITH a message was refused: %v", err)
		}
		if err := insert(ToolInvocationOK, nil); err != nil {
			t.Fatalf("a success with no message was refused: %v", err)
		}
	})

	t.Cleanup(func() {
		ctx := context.Background()
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return
		}
		defer c.Close(ctx)
		_, _ = c.Exec(ctx, `DELETE FROM tool_invocations WHERE tenant_id = 'int-013-check'`)
	})
}
