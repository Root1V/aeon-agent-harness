package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// isUniqueViolation reports whether err is a Postgres unique-constraint violation (SQLSTATE
// 23505) — used to turn a primary-key clash on Create into the domain-level ErrAlreadyExists
// instead of a raw driver error leaking out of the store package.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
