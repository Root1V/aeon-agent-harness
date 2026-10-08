package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// TestAMalformedMigrationSetStopsTheService covers the paths that must refuse at startup. They need
// no database: a bad set is a programming error, and the whole value of catching it here is that it
// happens before anything touches a schema.
func TestAMalformedMigrationSetStopsTheService(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "a file that is not NNNN_name.sql has no defined place in the history",
			files: map[string]string{"migrations/add_tenant.sql": "SELECT 1;"},
			want:  "does not match NNNN_snake_case_name.sql",
		},
		{
			name: "two files sharing a version",
			files: map[string]string{
				"migrations/0001_a.sql": "SELECT 1;",
				"migrations/0002_b.sql": "SELECT 1;",
				"migrations/0002_c.sql": "SELECT 1;",
			},
			want: "share version 2",
		},
		{
			// The dangerous one: a deployment may have applied the deleted migration, so it sits at a
			// version this binary has no record of — indistinguishable from a corrupted ledger.
			name: "a gap, which means one was deleted or never committed",
			files: map[string]string{
				"migrations/0001_a.sql": "SELECT 1;",
				"migrations/0003_c.sql": "SELECT 1;",
			},
			want: "not contiguous from 1",
		},
		{
			name:  "version 0 is reserved for \"none applied\"",
			files: map[string]string{"migrations/0000_zero.sql": "SELECT 1;"},
			want:  "version 0, which is reserved",
		},
		{
			name:  "an empty migration",
			files: map[string]string{"migrations/0001_a.sql": "\n\n"},
			want:  "is empty",
		},
		{
			// ANY stray file is refused, including a harmless-looking one. The cost is that a README
			// cannot live in this directory; the benefit is that `0002_add_tenant.sql.bak` or
			// `add_tenant.sql` cannot either — and those are files somebody believes are applied.
			// Silently skipping a misnamed migration is the failure this whole mechanism exists to
			// remove, so it is not made an exception for tidy-looking names.
			name:  "a stray file in the directory is refused rather than skipped",
			files: map[string]string{"migrations/README.md": "not sql"},
			want:  "does not match",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadMigrationsFrom(fstest.MapFS(asMapFS(tc.files)), "migrations")
			if err == nil {
				t.Fatal("the set loaded without complaint")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v\nwant it to mention %q", err, tc.want)
			}
		})
	}
}

func asMapFS(files map[string]string) map[string]*fstest.MapFile {
	out := map[string]*fstest.MapFile{}
	for name, body := range files {
		out[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return out
}

// TestTheEmbeddedSetIsValid is the one that would catch a bad migration being committed: the real
// embedded set, loaded with the real validation, every time the unit suite runs.
func TestTheEmbeddedSetIsValid(t *testing.T) {
	set, err := loadMigrations()
	if err != nil {
		t.Fatalf("the migrations this binary carries do not load: %v", err)
	}
	if len(set) == 0 {
		t.Fatal("no migrations embedded")
	}
	for _, m := range set {
		if len(m.Checksum) != 64 {
			t.Errorf("%04d_%s has a %d-character checksum", m.Version, m.Name, len(m.Checksum))
		}
	}
	t.Logf("%d migration(s), newest %04d_%s", len(set), set[len(set)-1].Version, set[len(set)-1].Name)
}

// TestMigrationsAreRecordedAndNotReapplied needs a real Postgres: every property worth asserting here
// is about what the database remembers between two calls.
func TestMigrationsAreRecordedAndNotReapplied(t *testing.T) {
	dsn := os.Getenv("AEON_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("AEON_TEST_PG_DSN not set — skipping Postgres integration test (see make test-go-integration)")
	}
	ctx := context.Background()
	s, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer s.Close()

	set, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	newest := set[len(set)-1].Version

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != newest {
		t.Fatalf("schema version = %d after Connect, want %d — Connect applies the migrations", version, newest)
	}

	// Connect already migrated; doing it again must be a no-op decided by the LEDGER. If it were
	// decided by the DDL being repeatable, a destructive migration would run twice.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	t.Run("an applied migration that was edited is refused", func(t *testing.T) {
		// The defect this prevents is silent by construction: the file says one thing, the database
		// holds what the old version did, and nothing compares them. So the checksum is compared.
		conn, err := s.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()

		edited := make([]migration, len(set))
		copy(edited, set)
		edited[0].Checksum = strings.Repeat("a", 64)

		err = applyMigrations(ctx, conn, edited)
		if err == nil {
			t.Fatal("an edited applied migration was accepted")
		}
		if !strings.Contains(err.Error(), "must never be edited") {
			t.Errorf("error = %v\nwant it to say the migration must not be edited", err)
		}
	})

	t.Run("a database ahead of the binary is refused", func(t *testing.T) {
		// A rollback mid-upgrade: a newer image migrated, this older one started. Querying a schema
		// this binary has no definition for is how a rollback becomes data written in a shape nothing
		// else expects — so it refuses instead.
		//
		// ITS OWN LEDGER TABLE, because the row this plants is precisely the one that makes Connect()
		// refuse. Planting it in the real ledger bricked every other test in the package until it was
		// deleted by hand, and the failure read as "the migration runner is broken" rather than "the
		// previous test left a row". A guard this effective needs a sandbox to be tested in.
		const ledger = "schema_migrations_ahead_test"
		t.Cleanup(func() {
			if _, err := s.pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+ledger); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		})
		conn, err := s.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()

		// Create the sandbox ledger the way the runner would, then plant the row.
		if err := applyMigrationsTo(ctx, conn, set, ledger); err != nil {
			t.Fatalf("priming the sandbox ledger: %v", err)
		}
		const fromTheFuture = 9999
		if _, err := conn.Exec(ctx,
			`INSERT INTO `+ledger+` (version, name, checksum) VALUES ($1, 'from_a_newer_image', 'x')`,
			fromTheFuture); err != nil {
			t.Fatal(err)
		}
		err = applyMigrationsTo(ctx, conn, set, ledger)
		if err == nil {
			t.Fatal("a database with an unknown applied migration was accepted")
		}
		if !strings.Contains(err.Error(), "NEWER than this code") {
			t.Errorf("error = %v\nwant it to say the schema is newer than the code", err)
		}
	})
}

// TestAFailedMigrationLeavesNothingBehind is the property the comment in migrations.go claims and the
// one a destructive change depends on: one transaction per migration, so a failure leaves a version
// BOUNDARY and not half of one.
//
// It also proves the mechanism can do what it was built for. VRT-AEON-005 needs PRIMARY KEY changes
// on seven tables, which the previous `CREATE TABLE IF NOT EXISTS` scheme skipped in silence — so
// this applies a real destructive migration (dropping and re-adding a primary key) to a scratch
// table and checks it took effect, rather than shipping the machinery unproven for its only use case.
func TestAFailedMigrationLeavesNothingBehind(t *testing.T) {
	dsn := os.Getenv("AEON_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("AEON_TEST_PG_DSN not set — skipping Postgres integration test (see make test-go-integration)")
	}
	ctx := context.Background()
	s, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer s.Close()

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	// Synthetic versions well past the real set, and removed afterwards: applyMigrations writes to
	// the real ledger, so a test that used low numbers would claim a version the repository does not
	// carry — and the next startup would refuse, correctly, as "the schema is newer than this code".
	const failing, succeeding = 9001, 9002

	// THE REAL SET IS PREPENDED, and the first version of this test did not do that — which made the
	// failing-migration subtest pass for the WRONG REASON. Passing only the synthetic migration means
	// 0001 is applied in the database and absent from the list, so applyMigrations refused with "the
	// schema is NEWER than this code" and the test, which only checked that SOME error came back, was
	// green over a guard firing instead of the statement failing. Both subtests now assert the
	// specific message.
	realSet, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	withSynthetic := func(extra migration) []migration {
		out := make([]migration, 0, len(realSet)+1)
		out = append(out, realSet...)
		return append(out, extra)
	}
	// Its own ledger, for the same reason as the subtest above: these synthetic versions must never
	// reach the real one, or the next startup refuses with a rollback message about a rollback that
	// never happened.
	const ledger = "schema_migrations_scratch_test"
	cleanup := func() {
		_, _ = s.pool.Exec(context.Background(), `DROP TABLE IF EXISTS migration_scratch`)
		_, _ = s.pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+ledger)
	}
	cleanup()
	t.Cleanup(cleanup)

	t.Run("a migration whose second statement fails applies neither", func(t *testing.T) {
		err := applyMigrationsTo(ctx, conn, withSynthetic(migration{
			Version:  failing,
			Name:     "scratch_then_fail",
			Checksum: strings.Repeat("b", 64),
			SQL: `CREATE TABLE migration_scratch (id TEXT NOT NULL, PRIMARY KEY (id));
			      SELECT this_function_does_not_exist();`,
		}), ledger)
		if err == nil {
			t.Fatal("a migration with a broken statement reported success")
		}
		// The SPECIFIC failure, not just any: see the note above about passing for the wrong reason.
		if !strings.Contains(err.Error(), "applying migration 9001_scratch_then_fail") {
			t.Fatalf("error = %v\nwant the failure to be the broken statement in 9001", err)
		}

		var exists bool
		if qerr := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'migration_scratch')`,
		).Scan(&exists); qerr != nil {
			t.Fatal(qerr)
		}
		if exists {
			t.Error("the first statement survived a failed migration — the schema is now in a state " +
				"no version describes, which is exactly what one transaction per migration prevents")
		}

		var recorded int
		if qerr := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM `+ledger+` WHERE version = $1`, failing).Scan(&recorded); qerr != nil {
			t.Fatal(qerr)
		}
		if recorded != 0 {
			t.Error("a failed migration was recorded as applied, so the next startup would skip it")
		}
	})

	t.Run("a destructive migration really changes a primary key", func(t *testing.T) {
		if _, err := s.pool.Exec(ctx,
			`CREATE TABLE migration_scratch (tenant_id TEXT NOT NULL DEFAULT '', id TEXT NOT NULL, PRIMARY KEY (id))`); err != nil {
			t.Fatal(err)
		}
		// Two rows that CANNOT coexist under the old key and must under the new one. That is the
		// whole shape of T-3: the same idempotency key in two tenants is two independent executions.
		if _, err := s.pool.Exec(ctx, `INSERT INTO migration_scratch (tenant_id, id) VALUES ('a', 'same-key')`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO migration_scratch (tenant_id, id) VALUES ('b', 'same-key')`); err == nil {
			t.Fatal("the old single-column key already allowed a duplicate — this test proves nothing")
		}

		if err := applyMigrationsTo(ctx, conn, withSynthetic(migration{
			Version:  succeeding,
			Name:     "scratch_tenant_key",
			Checksum: strings.Repeat("c", 64),
			SQL: `ALTER TABLE migration_scratch DROP CONSTRAINT migration_scratch_pkey;
			      ALTER TABLE migration_scratch ADD PRIMARY KEY (tenant_id, id);`,
		}), ledger); err != nil {
			t.Fatalf("the destructive migration failed: %v", err)
		}

		if _, err := s.pool.Exec(ctx, `INSERT INTO migration_scratch (tenant_id, id) VALUES ('b', 'same-key')`); err != nil {
			t.Fatalf("after the migration the same key in another tenant is still refused: %v", err)
		}
		var version int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+ledger+` WHERE version = $1`, succeeding).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != 1 {
			t.Error("the migration ran but was not recorded, so it would run again")
		}
	})
}
