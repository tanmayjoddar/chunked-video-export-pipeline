package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// NOTE: migrations live in ./migrations/*.sql relative to this package.
// They are ALSO duplicated at repo-root /migrations for operators.
// Migrate reads the embedded copies so the binary is self-sufficient.

// Migrate applies plain-SQL files in lexical order, idempotently
// (every file must use IF NOT EXISTS / idempotent DDL).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, n := range names {
		var applied bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name=$1)`, n).Scan(&applied)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", n, err)
		}
		if applied {
			continue
		}
		b, err := fs.ReadFile(migrationFS, "migrations/"+n)
		if err != nil {
			return fmt.Errorf("read %s: %w", n, err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("apply %s: %w", n, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES($1) ON CONFLICT DO NOTHING`, n); err != nil {
			return fmt.Errorf("record %s: %w", n, err)
		}
	}
	return nil
}
