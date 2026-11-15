package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/goware/pgqueue"
	"github.com/twmb/franz-go/pkg/kgo"

	"tradingbot/trade-events/internal/fill_event"
)

// Relay publishes committed outbox tasks to Kafka until ctx is canceled.
func (o *Outbox) Relay(ctx context.Context, producer *kgo.Client, topic string) error {
	worker := pgqueue.NewWorker(o.queue, pgqueue.WithReapInterval(5*time.Second))
	if err := pgqueue.Register(worker, publishJob, &relay{producer: producer, topic: topic}); err != nil {
		return err
	}
	return worker.Start(ctx)
}

type relay struct {
	producer *kgo.Client
	topic    string
}

func (r *relay) RunTask(ctx context.Context, job *pgqueue.Job[fill_event.Event]) pgqueue.Result {
	event := job.Payload
	if err := event.Validate(); err != nil {
		return pgqueue.Fail(fmt.Errorf("invalid fill event: %w", err))
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return pgqueue.Fail(fmt.Errorf("encode fill event: %w", err))
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	record := &kgo.Record{Topic: r.topic, Key: []byte(event.ID), Value: payload}
	if err := r.producer.ProduceSync(ctx, record).FirstErr(); err != nil {
		return pgqueue.Retry(fmt.Errorf("publish fill %s to Kafka: %w", event.ID, err))
	}
	slog.Info("fill event acknowledged by Kafka", "event_id", event.ID, "task_id", job.Task.ID,
		"topic", r.topic, "partition", record.Partition, "offset", record.Offset)
	return pgqueue.Done()
}
