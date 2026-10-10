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

	t.Run("an outcome nobody defined is refused", func(t *testing.T) {
		if err := insert("denied", nil); err == nil {
			t.Fatal(`'denied' was accepted: a denial never reaches Execute, so a row claiming one would ` +
				`assert something the gateway cannot observe here — see the roadmap row for that follow-up`)
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
