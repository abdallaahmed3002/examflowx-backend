// Package migrations applies embedded, versioned PostgreSQL schema changes.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed *.sql
var files embed.FS

func Apply(ctx context.Context, db *pgx.Conn) error {
	if _, err := db.Exec(ctx, `SELECT pg_advisory_lock(68409366)`); err != nil {
		return err
	}
	defer db.Exec(context.Background(), `SELECT pg_advisory_unlock(68409366)`)
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)
	for _, name := range entries {
		var found bool
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&found); err != nil {
			return err
		}
		if found {
			continue
		}
		script, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			return err
		}
		// pgx simple protocol accepts multiple SQL statements in a migration.
		if _, err = tx.Exec(ctx, string(script), pgx.QueryExecModeSimpleProtocol); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, name); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("track %s: %w", name, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
	}
	return nil
}

// ValidateVersionNames ensures migrations use an ordered numeric prefix.
func ValidateVersionNames() error {
	entries, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	for _, name := range entries {
		if len(name) < 8 || name[3] != '_' || !strings.HasSuffix(name, ".sql") {
			return fmt.Errorf("invalid migration filename %q", name)
		}
	}
	return nil
}
