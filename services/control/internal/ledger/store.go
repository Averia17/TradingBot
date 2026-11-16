// Package ledger owns balances and economic events for virtual paper accounts.
package ledger

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid  = errors.New("invalid paper ledger request")
	ErrNotFound = errors.New("paper ledger record not found")
	ErrConflict = errors.New("paper ledger record conflicts with existing state")
)

//go:embed 001_accounts.sql
var accountsMigration string

//go:embed 002_orders.sql
var ordersMigration string

type Store struct {
	pool *pgxpool.Pool
}

// Open verifies durable PostgreSQL storage and applies paper ledger migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("missing paper PostgreSQL configuration")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid paper PostgreSQL configuration")
	}
	if config.ConnConfig.ConnectTimeout <= 0 || config.ConnConfig.ConnectTimeout > 5*time.Second {
		config.ConnConfig.ConnectTimeout = 5 * time.Second
	}
	config.MaxConns = min(config.MaxConns, 4)
	if config.MinConns > config.MaxConns || config.MinIdleConns > config.MaxConns {
		return nil, errors.New("paper PostgreSQL minimum pool size exceeds the connection limit")
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("configure paper PostgreSQL pool")
	}
	store := &Store{pool: pool}
	if err := store.migrate(ctx); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) write(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin paper ledger transaction: %w", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = 'on'`); err != nil {
		rollback(tx)
		return nil, fmt.Errorf("configure paper ledger durability: %w", err)
	}
	return tx, nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.write(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var fsync string
	if err := tx.QueryRow(ctx, `SHOW fsync`).Scan(&fsync); err != nil || fsync != "on" {
		return errors.New("paper PostgreSQL durability requires fsync enabled")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(737291, 2)`); err != nil {
		return fmt.Errorf("lock paper migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS paper_schema_migrations (
		version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
	)`); err != nil {
		return fmt.Errorf("create paper migration history: %w", err)
	}
	var version int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM paper_schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("read paper schema version: %w", err)
	}
	migrations := []string{accountsMigration, ordersMigration}
	if version < 0 || version > len(migrations) {
		return fmt.Errorf("unsupported paper schema version %d", version)
	}
	for index := version; index < len(migrations); index++ {
		if _, err := tx.Exec(ctx, migrations[index], pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("apply paper migration %d: %w", index+1, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO paper_schema_migrations (version) VALUES ($1)`, index+1); err != nil {
			return fmt.Errorf("record paper migration %d: %w", index+1, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit paper migrations: %w", err)
	}
	return nil
}
