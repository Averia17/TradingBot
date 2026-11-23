package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goware/pgkit/v2"
	"github.com/goware/pgqueue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"tradingbot/trade-events/internal/fill_event"
)

const queueName = "trade-filled-v1"

var ErrConflict = errors.New("trade event ID already exists with different contents")

var publishJob = publishJobSpec[fill_event.Event](queueName)

func publishJobSpec[T any](queueName string) pgqueue.JobSpec[T] {
	return pgqueue.JobSpec[T]{
		Queue:          queueName,
		PollInterval:   time.Second,
		MaxRetries:     20,
		RetryDelay:     10 * time.Second,
		LeaseDuration:  45 * time.Second,
		FinalizeBuffer: 10 * time.Second,
	}
}

type Outbox struct {
	db    *pgkit.DB
	queue *pgqueue.Queue
}

// Open prepares pgqueue in the trade-events database.
func Open(ctx context.Context, dsn string) (*Outbox, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL outbox configuration")
	}
	config.MaxConns = 4
	config.MinConns = 0
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	config.ConnConfig.RuntimeParams["application_name"] = "tradingbot-trade-outbox"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	db, err := pgkit.ConnectWithPGX("tradingbot-trade-outbox", config)
	if err != nil {
		return nil, errors.New("initialize PostgreSQL outbox pool")
	}
	queue := pgqueue.New(db)
	// Serialize schema setup so multiple processes can start against a fresh DB.
	tx, err := db.Conn.Begin(ctx)
	if err != nil {
		db.Conn.Close()
		return nil, errors.New("connect to PostgreSQL outbox")
	}
	err = migrate(ctx, tx, queue)
	if err != nil {
		db.Conn.Close()
		return nil, err
	}
	return &Outbox{db: db, queue: queue}, nil
}

func migrate(ctx context.Context, tx pgx.Tx, queue *pgqueue.Queue) error {
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(ctx)
	}()
	var fsync string
	if err := tx.QueryRow(ctx, "SHOW fsync").Scan(&fsync); err != nil || fsync != "on" {
		return errors.New("trade-events PostgreSQL durability requires fsync enabled")
	}
	const migrationLock = 79077101940501201
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(migrationLock)); err != nil {
		return errors.New("lock PostgreSQL outbox migration")
	}
	if err := pgqueue.Migrate(ctx, queue); err != nil {
		return errors.New("migrate PostgreSQL outbox")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("release PostgreSQL outbox migration lock")
	}
	return nil
}

func (o *Outbox) Close() { o.db.Conn.Close() }

// Requeue restores a failed or quarantined trade task after the cause is fixed.
func (o *Outbox) Requeue(ctx context.Context, id int64) error {
	task, err := o.queue.Get(ctx, id)
	if err != nil {
		return err
	}
	if task.Queue != queueName && task.Queue != paperQueueName && task.Queue != paperAccountQueue {
		return pgqueue.ErrTaskNotFound
	}
	return o.queue.Enable(ctx, id)
}

// EnqueueTx is called after the authoritative fill write in the caller's pgx
// transaction. The caller must roll back that transaction if this call fails.
func (o *Outbox) EnqueueTx(ctx context.Context, tx pgx.Tx, event fill_event.Event) (int64, error) {
	event.FilledAt = event.FilledAt.UTC()
	if err := event.Validate(); err != nil {
		return 0, fmt.Errorf("validate confirmed fill event: %w", err)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return 0, fmt.Errorf("encode confirmed fill event: %w", err)
	}
	return enqueueExactTask(ctx, tx, queueName, event.ID, payload)
}

// The pgqueue enqueue API always looks up the ID after a deduplicated insert.
// Our byte comparison is needed only when a task already exists.
func enqueueExactTask(ctx context.Context, tx pgx.Tx, queueName, dedupKey string, payload []byte) (int64, error) {
	var taskID int64
	err := tx.QueryRow(ctx, `INSERT INTO pgqueue_tasks (queue, hash, payload, created_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT (queue, hash) DO NOTHING RETURNING id`,
		queueName, dedupKey, payload, time.Now().UTC()).Scan(&taskID)
	if err == nil {
		return taskID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("enqueue outbox task: %w", err)
	}
	// A separate statement sees the winner of a concurrent insert under
	// READ COMMITTED, including one that committed while this insert waited.
	var samePayload bool
	if err := tx.QueryRow(ctx, `SELECT id, payload IS NOT DISTINCT FROM $3::bytea
		FROM pgqueue_tasks WHERE queue = $1 AND hash = $2`,
		queueName, dedupKey, payload).Scan(&taskID, &samePayload); err != nil {
		return 0, fmt.Errorf("read existing outbox task: %w", err)
	}
	if !samePayload {
		return 0, ErrConflict
	}
	return taskID, nil
}
