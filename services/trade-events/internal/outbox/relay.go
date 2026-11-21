package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/goware/pgqueue"
	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kgo"
	"golang.org/x/sync/errgroup"

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

func runHandoff(ctx context.Context, worker *pgqueue.Worker, name string, queueOne func(context.Context) (bool, error)) error {
	group, runCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		if err := worker.Start(runCtx); err != nil {
			return err
		}
		if runCtx.Err() == nil {
			return fmt.Errorf("%s relay stopped unexpectedly", name)
		}
		return nil
	})
	group.Go(func() error {
		for runCtx.Err() == nil {
			moved, err := queueOne(runCtx)
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

func (o *Outbox) handoff(ctx context.Context, name string, move func(context.Context, pgx.Tx) (bool, error)) (bool, error) {
	tx, err := o.db.Conn.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin %s handoff: %w", name, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit='on'`); err != nil {
		return false, fmt.Errorf("configure durable %s handoff: %w", name, err)
	}
	moved, err := move(ctx, tx)
	if err != nil || !moved {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit %s handoff: %w", name, err)
	}
	committed = true
	return true, nil
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
	return r.publish(ctx, job.Task.ID, event.ID, event.ID, payload)
}

func (r *relay) publish(ctx context.Context, taskID int64, id, key string, payload []byte) pgqueue.Result {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	record := &kgo.Record{Topic: r.topic, Key: []byte(key), Value: payload}
	if err := r.producer.ProduceSync(ctx, record).FirstErr(); err != nil {
		return pgqueue.Retry(fmt.Errorf("publish fill %s to Kafka: %w", id, err))
	}
	slog.Info("fill event acknowledged by Kafka", "event_id", id, "task_id", taskID,
		"topic", r.topic, "partition", record.Partition, "offset", record.Offset)
	return pgqueue.Done()
}
