package store

import (
	"context"
	"os"
	"testing"
)

// testDSN returns AEON_TEST_PG_DSN, or skips the calling test if it isn't set. Registry tests are
// integration tests against a real Postgres (see deploy/compose/docker-compose.yml, service
// "postgres") — there is no mocked/in-memory substitute, because the point of this package is the
// real SQL constraints (unique versions, lifecycle CHECK) doing their job.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AEON_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("AEON_TEST_PG_DSN not set — skipping Postgres integration test (see make test-go-integration)")
	}
	return dsn
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}
