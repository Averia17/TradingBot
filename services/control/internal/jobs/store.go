// Package jobs persists research jobs and their immutable inputs in PostgreSQL.
package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("research job not found")
	ErrConflict = errors.New("research job or snapshot conflicts with existing content")
)

//go:embed 001_research_jobs.sql
var researchJobsMigration string

//go:embed 002_research_leases.sql
var researchLeasesMigration string

//go:embed 003_research_results.sql
var researchResultsMigration string

//go:embed 004_research_call_budget.sql
var researchCallBudgetMigration string

//go:embed 005_paper_dispatch.sql
var researchPaperDispatchMigration string

//go:embed 006_research_daily_call_budget.sql
var researchDailyCallBudgetMigration string

type Store struct {
	pool *pgxpool.Pool
}

type Job struct {
	ID        string
	Request   json.RawMessage
	Status    string
	CreatedAt time.Time
}

// CreateJob contains HTTP-validated canonical JSON for the versioned request
// and its snapshot. Their exact bytes are persisted and compared on retry.
type CreateJob struct {
	ID            string
	Request       json.RawMessage
	SnapshotID    string
	Snapshot      json.RawMessage
	PaperDispatch *PaperDispatchInput
}

// Open connects to PostgreSQL and applies the schema before returning a store.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("missing PostgreSQL connection configuration")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	if timeout := config.ConnConfig.ConnectTimeout; timeout <= 0 || timeout > 5*time.Second {
		config.ConnConfig.ConnectTimeout = 5 * time.Second
	}
	config.MaxConns = min(config.MaxConns, 8)
	if config.MinConns > config.MaxConns || config.MinIdleConns > config.MaxConns {
		return nil, errors.New("PostgreSQL minimum pool size exceeds the connection limit")
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("configure PostgreSQL connection pool")
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

// Now returns the same accounting clock used to timestamp research receipts.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var at time.Time
	if err := s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("read research storage clock: %w", err)
	}
	return at.UTC(), nil
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return errors.New("connect to PostgreSQL for research job migration")
	}
	defer rollback(tx)
	var fsync string
	if err := tx.QueryRow(ctx, `SHOW fsync`).Scan(&fsync); err != nil {
		return fmt.Errorf("check PostgreSQL durability: %w", err)
	}
	if fsync != "on" {
		return errors.New("PostgreSQL fsync must be enabled for durable research jobs")
	}
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = 'on'`); err != nil {
		return fmt.Errorf("configure durable research schema commit: %w", err)
	}
	// Serialize migrations between control processes on the same database.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(737291, 1)`); err != nil {
		return fmt.Errorf("lock research schema migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS research_schema_migrations (
		version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
	)`); err != nil {
		return fmt.Errorf("prepare research schema migrations: %w", err)
	}
	var version int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM research_schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("read research schema version: %w", err)
	}
	migrations := []string{
		researchJobsMigration, researchLeasesMigration, researchResultsMigration,
		researchCallBudgetMigration, researchPaperDispatchMigration, researchDailyCallBudgetMigration,
		researchCalendarMigration, researchCallUsageMigration,
	}
	if version < 0 || version > len(migrations) {
		return fmt.Errorf("unsupported research database schema version %d", version)
	}
	for index := version; index < len(migrations); index++ {
		next := index + 1
		if _, err := tx.Exec(ctx, migrations[index], pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("apply research migration %d: %w", next, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO research_schema_migrations (version) VALUES ($1)`, next); err != nil {
			return fmt.Errorf("record research schema version %d: %w", next, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit research schema migration: %w", err)
	}
	return nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	row := s.pool.QueryRow(ctx, `SELECT id::text, request, status, created_at, request_sha256 FROM research_jobs WHERE id = $1`, id)
	var job Job
	var digest []byte
	if err := row.Scan(&job.ID, &job.Request, &job.Status, &job.CreatedAt, &digest); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Job{}, ErrNotFound
		}
		return Job{}, fmt.Errorf("read research job: %w", err)
	}
	expected := sha256.Sum256(job.Request)
	if !bytes.Equal(expected[:], digest) {
		return Job{}, errors.New("persisted research job does not match its hash")
	}
	job.CreatedAt = job.CreatedAt.UTC()
	return job, nil
}

// Create commits a new queued job with its snapshot and outbox notification.
// admit must be a short local check. It runs only before committing a new job.
// A duplicate is resolved against committed data without reapplying admission.
func (s *Store) Create(ctx context.Context, input CreateJob, admit func() error) (Job, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Job{}, false, fmt.Errorf("begin research job transaction: %w", err)
	}
	defer rollback(tx)
	requestHash := sha256.Sum256(input.Request)
	snapshotHash := sha256.Sum256(input.Snapshot)
	if job, err := existingJob(ctx, tx, input, requestHash[:], snapshotHash[:]); !errors.Is(err, ErrNotFound) {
		return job, false, err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = 'on'`); err != nil {
		return Job{}, false, fmt.Errorf("configure durable research job commit: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO research_snapshots (id, payload, payload_sha256) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO NOTHING`, input.SnapshotID, []byte(input.Snapshot), snapshotHash[:]); err != nil {
		return Job{}, false, fmt.Errorf("store research snapshot: %w", err)
	}
	var sameSnapshot bool
	if err := tx.QueryRow(ctx, `SELECT payload_sha256 = $2 AND payload = $3 FROM research_snapshots WHERE id = $1`,
		input.SnapshotID, snapshotHash[:], []byte(input.Snapshot)).Scan(&sameSnapshot); err != nil {
		return Job{}, false, fmt.Errorf("verify research snapshot identity: %w", err)
	}
	if !sameSnapshot {
		return Job{}, false, ErrConflict
	}
	job := Job{ID: input.ID, Request: input.Request}
	err = tx.QueryRow(ctx, `INSERT INTO research_jobs (id, snapshot_id, request, request_sha256) VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO NOTHING RETURNING status, created_at`, input.ID, input.SnapshotID, []byte(input.Request), requestHash[:]).Scan(&job.Status, &job.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another transaction committed this ID after our first read. Use a new
		// READ COMMITTED statement before the deferred rollback of our writes.
		job, err := existingJob(ctx, tx, input, requestHash[:], snapshotHash[:])
		return job, false, err
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("store research job: %w", err)
	}
	if admit != nil {
		if err := admit(); err != nil {
			return Job{}, false, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO research_outbox (job_id, event_type, created_at) VALUES ($1, 'research_job.queued', $2)`,
		job.ID, job.CreatedAt); err != nil {
		return Job{}, false, fmt.Errorf("store queued research job event: %w", err)
	}
	if input.PaperDispatch != nil {
		if err := insertPaperDispatch(ctx, tx, job.ID, *input.PaperDispatch); err != nil {
			return Job{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, false, fmt.Errorf("commit research job: %w", err)
	}
	job.CreatedAt = job.CreatedAt.UTC()
	return job, true, nil
}

func existingJob(ctx context.Context, tx pgx.Tx, input CreateJob, requestHash, snapshotHash []byte) (Job, error) {
	var job Job
	var matched bool
	err := tx.QueryRow(ctx, `SELECT j.id::text, j.request, j.status, j.created_at,
		j.request_sha256 = $2 AND s.id = $3 AND s.payload_sha256 = $4 AND s.payload = $5
		FROM research_jobs j JOIN research_snapshots s ON j.snapshot_id = s.id WHERE j.id = $1`,
		input.ID, requestHash, input.SnapshotID, snapshotHash, []byte(input.Snapshot)).Scan(&job.ID, &job.Request, &job.Status, &job.CreatedAt, &matched)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("look up research job idempotency: %w", err)
	}
	if !matched || !bytes.Equal(job.Request, input.Request) {
		return Job{}, ErrConflict
	}
	if input.PaperDispatch != nil {
		dispatch, err := readPaperDispatch(tx.QueryRow(ctx, selectPaperDispatch, input.ID))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Job{}, err
		}
		if err == nil && (dispatch.AccountID != input.PaperDispatch.AccountID || !bytes.Equal(dispatch.Request, input.PaperDispatch.Request)) {
			return Job{}, ErrConflict
		}
	}
	job.CreatedAt = job.CreatedAt.UTC()
	return job, nil
}
