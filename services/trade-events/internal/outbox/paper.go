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

	"tradingbot/trade-events/internal/fill_event"
)

const paperQueueName = "paper-trade-filled-v1"

var publishPaperJob = publishJobSpec[json.RawMessage](paperQueueName)

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
	return runHandoff(ctx, worker, "paper fill", o.queueOnePaper)
}

func (o *Outbox) queueOnePaper(ctx context.Context) (bool, error) {
	return o.handoff(ctx, "paper outbox", o.movePaperFill)
}

func (o *Outbox) movePaperFill(ctx context.Context, tx pgx.Tx) (bool, error) {
	var accountID, eventID string
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT account_id::text,event_id::text,payload FROM paper_fill_outbox WHERE queued_at IS NULL
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
	canonicalPayload, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("encode paper fill outbox task: %w", err)
	}
	if _, err := enqueueExactTask(ctx, tx, paperQueueName, event.ID, canonicalPayload); err != nil {
		return false, err
	}
	mark, err := tx.Exec(ctx, `UPDATE paper_fill_outbox SET queued_at=clock_timestamp() WHERE account_id=$1 AND event_id=$2`, accountID, eventID)
	if err != nil {
		return false, fmt.Errorf("mark ledger fill queued: %w", err)
	}
	if mark.RowsAffected() != 1 {
		return false, errors.New("paper fill handoff did not update exactly one outbox row")
	}
	return true, nil
}

type paperRelay struct{ relay }

func (r *paperRelay) RunTask(ctx context.Context, job *pgqueue.Job[json.RawMessage]) pgqueue.Result {
	fill, err := fill_event.DecodePaper(job.Payload)
	if err != nil {
		return pgqueue.Fail(fmt.Errorf("invalid paper fill: %w", err))
	}
	// The handoff stored canonical bytes in the same transaction as queued_at.
	return r.publish(ctx, job.Task.ID, fill.ID, fill.ID, job.Payload)
}
