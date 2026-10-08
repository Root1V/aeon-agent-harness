package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Versioned schema migrations (VRT-AEON-005's prerequisite).
//
// WHY THIS EXISTS NOW AND NOT BEFORE. Until 2026-10-07 the schema was one idempotent file applied at
// startup, and that was honestly sufficient: every change had been ADDITIVE, and
// `CREATE TABLE/INDEX IF NOT EXISTS` applies an additive change correctly however many times it
// runs. The header of what is now 0001 said so in as many words.
//
// Multi-tenancy is the first change that is not additive. Scoping deduplication and the registries
// to a tenant means changing PRIMARY KEYs and UNIQUE constraints on seven tables that already hold
// data, and `CREATE TABLE IF NOT EXISTS` does not alter an existing table — it SKIPS it, without
// saying so. The failure mode is the one the constraint existed to prevent: a deployment that
// believes deduplication is tenant-scoped, is not, and finds out when one tenant's step is treated
// as already executed because another tenant used the same key.
//
// WHAT THIS IS NOT: golang-migrate or atlas. Those are good and this is deliberately smaller —
// ordered files, a ledger table, checksums, one transaction each, forward only. The backlog entry
// that asked for this noted that a bespoke `schema_version` "parece más barato y es cómo se acaba
// teniendo una herramienta de migraciones peor", and that risk is real. What keeps it honest is the
// list of things it refuses (below) rather than the list of features it has.
//
// NO DOWN MIGRATIONS, and that is a decision rather than an omission. A down migration for an
// additive change is easy and useless; for a destructive one it cannot restore the data the up
// migration dropped, so shipping a `down` would promise a reversibility that does not exist and
// invite someone to rely on it during an incident. Recovery is a restore plus a roll forward.

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationNamePattern fixes the file naming so ordering is a property of the names and not of
// whoever ran `ls`: four digits, an underscore, a snake_case description, `.sql`.
var migrationNamePattern = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// ledgerTablePattern is the belt to the braces above: only a plain identifier can name the ledger.
var ledgerTablePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

type migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// loadMigrations reads and validates the embedded set. Every failure here is a programming error
// caught at startup rather than a half-applied schema later: a gap or a duplicate version means the
// author of a new migration did not look at what was already there, and applying them anyway would
// make "version 4" mean different things in two deployments.
func loadMigrations() ([]migration, error) {
	return loadMigrationsFrom(migrationFS, "migrations")
}

// loadMigrationsFrom is loadMigrations with the source injected, so every validation below is
// testable without a database and without writing files — these are the paths that must stop a
// service at startup, and a guard whose refusals are untested is a guard nobody has seen refuse.
func loadMigrationsFrom(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("store: reading embedded migrations: %w", err)
	}

	var out []migration
	seen := map[int]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := migrationNamePattern.FindStringSubmatch(entry.Name())
		if m == nil {
			return nil, fmt.Errorf(
				"store: migration file %q does not match NNNN_snake_case_name.sql — the number is how "+
					"order is decided, so a file without one has no defined place in the history", entry.Name())
		}
		version, _ := strconv.Atoi(m[1])
		if version == 0 {
			return nil, fmt.Errorf("store: migration %q uses version 0, which is reserved for \"none applied\"", entry.Name())
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", prev, entry.Name(), version)
		}
		seen[version] = entry.Name()

		body, err := fs.ReadFile(fsys, dir+"/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("store: reading migration %q: %w", entry.Name(), err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("store: migration %q is empty", entry.Name())
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			Version:  version,
			Name:     m[2],
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	// CONTIGUOUS FROM 1, checked rather than assumed. A gap means a migration was deleted or never
	// committed, and a deployment that applied the missing one is now at a version this binary has
	// no record of — which is the state the ledger check below cannot distinguish from corruption.
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf(
				"store: migration versions are not contiguous from 1: expected %04d, found %04d (%s). "+
					"A gap means one was deleted or never committed", i+1, m.Version, m.Name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("store: no migrations are embedded, so this binary carries no schema")
	}
	return out, nil
}

// defaultLedgerTable is where applied migrations are recorded. It is a parameter rather than a
// constant inside applyMigrations for one reason: a test that has to simulate "the database is ahead
// of this binary" must plant a row the binary does not know, and planting it in the REAL ledger
// bricks every other test in the package — Connect() then refuses, correctly, and the whole suite
// fails with a message about a rollback that never happened. Measured: it happened here, and the
// leftover row survived the run that created it.
const defaultLedgerTable = "schema_migrations"

const migrationLedgerDDLTemplate = `
CREATE TABLE IF NOT EXISTS %s (
    version     INTEGER NOT NULL PRIMARY KEY,
    name        TEXT NOT NULL,
    checksum    TEXT NOT NULL,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);`

// applyMigrations brings the database up to the version this binary carries.
//
// Called with the advisory lock already held (see Migrate), so two services starting at once do not
// race: one applies, the other waits and then finds nothing to do.
func applyMigrations(ctx context.Context, conn *pgxpool.Conn, pending []migration) error {
	return applyMigrationsTo(ctx, conn, pending, defaultLedgerTable)
}

func applyMigrationsTo(ctx context.Context, conn *pgxpool.Conn, pending []migration, ledger string) error {
	// The table name is interpolated and never a bind parameter, because an identifier cannot be one.
	// It is safe here for a reason worth stating rather than assuming: the only values that reach it
	// are this package's own constants and a test's literal, never anything from a request.
	if !ledgerTablePattern.MatchString(ledger) {
		return fmt.Errorf("store: %q is not a valid ledger table name", ledger)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(migrationLedgerDDLTemplate, ledger)); err != nil {
		return fmt.Errorf("store: creating the migration ledger: %w", err)
	}

	applied := map[int]struct {
		name     string
		checksum string
	}{}
	rows, err := conn.Query(ctx, fmt.Sprintf(`SELECT version, name, checksum FROM %s`, ledger))
	if err != nil {
		return fmt.Errorf("store: reading the migration ledger: %w", err)
	}
	for rows.Next() {
		var v int
		var name, checksum string
		if err := rows.Scan(&v, &name, &checksum); err != nil {
			rows.Close()
			return fmt.Errorf("store: scanning the migration ledger: %w", err)
		}
		applied[v] = struct {
			name     string
			checksum string
		}{name, checksum}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: reading the migration ledger: %w", err)
	}

	known := map[int]bool{}
	for _, m := range pending {
		known[m.Version] = true
	}

	// THE DATABASE AHEAD OF THE BINARY IS A REFUSAL, not something to roll forward from. It means a
	// newer deployment ran and this one is older — a rollback mid-upgrade — so this binary is about
	// to issue queries against a schema it has no definition for. Starting anyway is how a rollback
	// turns into data written in a shape nothing else expects.
	for v, a := range applied {
		if !known[v] {
			return fmt.Errorf(
				"store: the database has migration %04d (%s) applied and this binary does not carry it. "+
					"The schema is NEWER than this code — most likely a rollback to an older image. Refusing "+
					"to start rather than query a schema this binary has no definition for", v, a.name)
		}
	}

	for _, m := range pending {
		a, done := applied[m.Version]
		if done {
			// AN APPLIED MIGRATION THAT CHANGED ON DISK IS A REFUSAL. Editing one that already ran is
			// how the schema in the database and the schema in the repository diverge while every
			// test passes: the file says one thing, the database holds another, and nothing compares
			// them. Write a new migration instead — that is the entire reason the ledger stores a
			// checksum rather than just a version number.
			if a.checksum != m.Checksum {
				return fmt.Errorf(
					"store: migration %04d_%s was already applied with checksum %s but now hashes to %s. "+
						"An applied migration must never be edited: the database still holds what the old "+
						"version did. Add a new migration for the change you wanted",
					m.Version, m.Name, a.checksum[:12], m.Checksum[:12])
			}
			continue
		}

		// ONE TRANSACTION PER MIGRATION, so a failure leaves a version boundary and not half of one.
		// Postgres runs DDL transactionally, which is what makes this possible at all and is worth
		// knowing: the same code against MySQL would leave partial DDL behind on failure.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("store: begin migration %04d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: applying migration %04d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %s (version, name, checksum) VALUES ($1, $2, $3)`, ledger),
			m.Version, m.Name, m.Checksum); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: recording migration %04d_%s: %w", m.Version, m.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("store: committing migration %04d_%s: %w", m.Version, m.Name, err)
		}
	}
	return nil
}

// SchemaVersion reports the highest applied migration, or 0 when none are. Operational read: it is
// what answers "which schema is this database on" without inferring it from the presence of a column.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version *int
	err := s.pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("store: reading schema version: %w", err)
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}
