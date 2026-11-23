package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goware/pgqueue"
	"github.com/twmb/franz-go/pkg/kgo"

	"tradingbot/trade-events/internal/fill_event"
)

const paperQueueName = "paper-trade-filled-v1"

var publishPaperJob = publishJobSpec[json.RawMessage](paperQueueName)

// RelayPaper queues committed fills from Accounts over HTTP and publishes them to Kafka.
func (o *Outbox) RelayPaper(ctx context.Context, producer *kgo.Client, source *PaperSource) error {
	worker := pgqueue.NewWorker(o.queue, pgqueue.WithReapInterval(5*time.Second))
	handler := &paperRelay{relay{producer: producer, topic: fill_event.PaperTopic}}
	if err := pgqueue.Register(worker, publishPaperJob, handler); err != nil {
		return err
	}
	return runHandoff(ctx, worker, "paper fill", func(ctx context.Context) (bool, error) {
		return o.queueOnePaper(ctx, source)
	})
}

func (o *Outbox) queueOnePaper(ctx context.Context, source *PaperSource) (bool, error) {
	entry, err := source.next(ctx, "fills")
	if err != nil || entry == nil {
		return false, err
	}
	event, err := fill_event.DecodePaper(entry.Payload)
	if err != nil || entry.AccountVersion != 0 || event.AccountID != entry.AccountID || event.FillEventID != entry.EventID {
		return false, errors.New("invalid account outbox fill identity")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("encode paper fill outbox task: %w", err)
	}
	if err := o.handoff(ctx, paperQueueName, event.ID, payload); err != nil {
		return false, err
	}
	// After a lost ACK the source may return the same entry. The durable exact queue key handles retries.
	if err := source.ack(ctx, "/fills/"+event.AccountID+"/"+event.FillEventID); err != nil {
		return false, err
	}
	return true, nil
}

type paperRelay struct{ relay }

func (r *paperRelay) RunTask(ctx context.Context, job *pgqueue.Job[json.RawMessage]) pgqueue.Result {
	fill, err := fill_event.DecodePaper(job.Payload)
	if err != nil {
		return pgqueue.Fail(fmt.Errorf("invalid paper fill: %w", err))
	}
	return r.publish(ctx, job.Task.ID, fill.ID, fill.ID, job.Payload)
}
