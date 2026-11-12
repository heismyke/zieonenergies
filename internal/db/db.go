// Package db owns the PostgreSQL pool, schema migrations and the transaction
// helpers that bind every unit of work to a tenant for Row-Level Security.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 15
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Tenant runs fn in a transaction bound to tenantID. Inside it the session role
// is zieon_app, so RLS policies restrict every statement to that tenant even if
// a query forgets its own tenant_id predicate.
func (s *Store) Tenant(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	if tenantID == uuid.Nil {
		return errors.New("db: tenant transaction without tenant id")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE zieon_app`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
			return err
		}
		return fn(tx)
	})
}

// System runs fn with RLS bypassed. It is reserved for platform-administrator
// operations, pre-authentication lookups (API keys, device identity) and
// background workers. Every caller must be reviewed for tenant safety.
func (s *Store) System(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE zieon_app`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.bypass_rls', 'on', true)`); err != nil {
			return err
		}
		return fn(tx)
	})
}

func (s *Store) tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IsUniqueViolation reports whether err is a unique-constraint failure,
// optionally on a specific constraint/index name.
func IsUniqueViolation(err error, constraint ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	if len(constraint) == 0 {
		return true
	}
	for _, c := range constraint {
		if pgErr.ConstraintName == c {
			return true
		}
	}
	return false
}

// Migrate applies pending *.up.sql migrations in order, each in its own transaction.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	if _, err := s.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	// Serialise concurrent instances.
	if _, err := s.Pool.Exec(ctx, `SELECT pg_advisory_lock(727001)`); err != nil {
		return nil, err
	}
	defer s.Pool.Exec(context.Background(), `SELECT pg_advisory_unlock(727001)`) //nolint:errcheck

	files, err := migrationFiles(".up.sql")
	if err != nil {
		return nil, err
	}
	var applied []string
	for _, f := range files {
		version := strings.TrimSuffix(f, ".up.sql")
		var exists bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&exists); err != nil {
			return applied, err
		}
		if exists {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + f)
		if err != nil {
			return applied, err
		}
		err = s.tx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", version, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return applied, err
		}
		applied = append(applied, version)
	}
	return applied, nil
}

// MigrateDown rolls back the most recent migration.
func (s *Store) MigrateDown(ctx context.Context) (string, error) {
	var version string
	err := s.Pool.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	body, err := migrationFS.ReadFile("migrations/" + version + ".down.sql")
	if err != nil {
		return "", fmt.Errorf("no down migration for %s: %w", version, err)
	}
	return version, s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version=$1`, version)
		return err
	})
}

func migrationFiles(suffix string) ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
