package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goware/pgqueue"
	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kgo"
	"golang.org/x/sync/errgroup"

	"tradingbot/trade-events/internal/fill_event"
)

const paperQueueName = "paper-trade-filled-v1"

var publishPaperJob = pgqueue.JobSpec[fill_event.PaperEvent]{
	Queue: paperQueueName,
	HashFn: func(event fill_event.PaperEvent) *string {
		return &event.ID
	},
	PollInterval:   publishJob.PollInterval,
	MaxRetries:     publishJob.MaxRetries,
	RetryDelay:     publishJob.RetryDelay,
	LeaseDuration:  publishJob.LeaseDuration,
	FinalizeBuffer: publishJob.FinalizeBuffer,
}

// RelayPaper copies committed ledger fills to pgqueue and relays them to the
// dedicated Kafka topic. Both queue schemas must be in the ledger database.
func (o *Outbox) RelayPaper(ctx context.Context, producer *kgo.Client) error {
	if _, err := o.db.Conn.Exec(ctx, `SELECT account_id,event_id,payload,created_at,queued_at FROM paper_fill_outbox LIMIT 0`); err != nil {
		return errors.New("paper fill outbox is not accessible in the configured PostgreSQL schema")
	}
	worker := pgqueue.NewWorker(o.queue, pgqueue.WithReapInterval(5*time.Second))
	handler := &paperRelay{relay{producer: producer, topic: fill_event.PaperTopic}}
	if err := pgqueue.Register(worker, publishPaperJob, handler); err != nil {
		return err
	}
	group, runCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		if err := worker.Start(runCtx); err != nil {
			return err
		}
		if runCtx.Err() == nil {
			return errors.New("paper fill relay stopped unexpectedly")
		}
		return nil
	})
	group.Go(func() error {
		for runCtx.Err() == nil {
			moved, err := o.queueOnePaper(runCtx)
			if err != nil {
				return err
			}
			if !moved {
				select {
				case <-runCtx.Done():
				case <-time.After(time.Second):
				}
			}
		}
		return nil
	})
	return group.Wait()
}

func (o *Outbox) queueOnePaper(ctx context.Context) (bool, error) {
	tx, err := o.db.Conn.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin paper outbox handoff: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit='on'`); err != nil {
		return false, fmt.Errorf("configure durable paper outbox handoff: %w", err)
	}
	var accountID, eventID string
	var payload []byte
	err = tx.QueryRow(ctx, `SELECT account_id::text,event_id::text,payload FROM paper_fill_outbox WHERE queued_at IS NULL
		ORDER BY created_at,account_id,event_id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&accountID, &eventID, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select pending paper fill: %w", err)
	}
	event, err := fill_event.DecodePaper(payload)
	if err != nil || event.AccountID != accountID || event.FillEventID != eventID {
		return false, fmt.Errorf("invalid ledger outbox fill %s for account %s", eventID, accountID)
	}
	if _, err := enqueueExact(ctx, o.queue, tx, publishPaperJob, event); err != nil {
		return false, err
	}
	mark, err := tx.Exec(ctx, `UPDATE paper_fill_outbox SET queued_at=clock_timestamp() WHERE account_id=$1 AND event_id=$2`, accountID, eventID)
	if err != nil {
		return false, fmt.Errorf("mark ledger fill queued: %w", err)
	}
	if mark.RowsAffected() != 1 {
		return false, errors.New("paper fill handoff did not update exactly one outbox row")
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit paper outbox handoff: %w", err)
	}
	return true, nil
}

type paperRelay struct{ relay }

func (r *paperRelay) RunTask(ctx context.Context, job *pgqueue.Job[fill_event.PaperEvent]) pgqueue.Result {
	payload, err := json.Marshal(job.Payload)
	if err != nil {
		return pgqueue.Fail(fmt.Errorf("encode paper fill: %w", err))
	}
	if _, err := fill_event.DecodePaper(payload); err != nil {
		return pgqueue.Fail(fmt.Errorf("invalid paper fill: %w", err))
	}
	return r.publish(ctx, job.Task.ID, job.Payload.ID, payload)
}
