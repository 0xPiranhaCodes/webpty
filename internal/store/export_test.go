package store

import (
	"context"
	"database/sql"
)

// OpenRaw opens path with the production pragmas but without migrating.
func OpenRaw(path string) (*sql.DB, error) { return sql.Open("sqlite", dsn(path)) }

// MigrateTo applies migrations up to and including version on an unmigrated
// database file, so tests can seed data that later migrations must preserve.
func MigrateTo(ctx context.Context, path string, version int) error {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return err
	}
	defer db.Close()
	return migrateTo(ctx, db, version)
}
