package outbox

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/goware/pgqueue"
	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"tradingbot/trade-events/internal/fill_event"
)

func TestPaperLedgerHandoffAndKafkaAcknowledgement(t *testing.T) {
	ctx, dsn, db := testPostgres(t)
	for _, migration := range []string{"001_accounts.sql", "002_orders.sql", "003_partial_fills.sql", "004_fill_outbox.sql"} {
		sql, err := os.ReadFile("../../../control/internal/ledger/" + migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, string(sql), pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile("../../../../contracts/valid-paper-fill-event.json")
	if err != nil {
		t.Fatal(err)
	}
	event, err := fill_event.DecodePaper(data)
	if err != nil {
		t.Fatal(err)
	}
	box, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO paper_accounts (account_id,creation_hash,cash_usd,version)
		VALUES ($1,decode(repeat('00',32),'hex'),19,3)`, event.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_orders (account_id,order_id,decision_id,quote_id,request,request_hash,status,account_version,created_at)
		VALUES ($1,$2,$3,$4,'{}',decode(repeat('00',32),'hex'),'filled',3,$5)`, event.AccountID, event.OrderID, event.DecisionID, event.QuoteID, event.FilledAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_events (account_id,event_id,account_version,kind,request_hash,payload,recorded_at)
		VALUES ($1,$2,3,'paper_order.filled',decode(repeat('00',32),'hex'),'{}',$3)`, event.AccountID, event.FillEventID, event.RecordedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_fills (account_id,order_id,event_id,gross_usd,fee_usd,observed_at,quantity_raw)
		VALUES ($1,$2,$3,1,0.00000001,$4,3)`, event.AccountID, event.OrderID, event.FillEventID, event.FilledAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_fill_outbox (account_id,event_id,payload,created_at) VALUES ($1,$2,$3,$4)`,
		event.AccountID, event.FillEventID, data, event.RecordedAt); err != nil {
		t.Fatal(err)
	}
	if queued, err := box.queueOnePaper(ctx); err != nil || queued {
		t.Fatalf("uncommitted fill was moved to the relay: queued=%v err=%v", queued, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(ctx, `ALTER TABLE paper_fill_outbox ADD CONSTRAINT test_reject_handoff CHECK (queued_at IS NULL)`); err != nil {
		t.Fatal(err)
	}
	if queued, err := box.queueOnePaper(ctx); err == nil || queued {
		t.Fatalf("failed handoff was acknowledged: queued=%v err=%v", queued, err)
	}
	var tasks int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pgqueue_tasks`).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("handoff failure left a queue task: tasks=%d err=%v", tasks, err)
	}
	if _, err := db.Exec(ctx, `ALTER TABLE paper_fill_outbox DROP CONSTRAINT test_reject_handoff`); err != nil {
		t.Fatal(err)
	}
	var handoffs sync.WaitGroup
	results := make(chan bool, 4)
	for range 4 {
		handoffs.Go(func() {
			queued, err := box.queueOnePaper(ctx)
			if err != nil {
				t.Error(err)
			}
			results <- queued
		})
	}
	handoffs.Wait()
	close(results)
	moved := 0
	for queued := range results {
		if queued {
			moved++
		}
	}
	if moved != 1 {
		t.Fatalf("concurrent handoff created %d tasks", moved)
	}

	// A fresh process with no in-memory state sends the durable pgqueue entry.
	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var taskID int64
	if err := db.QueryRow(ctx, `SELECT id FROM pgqueue_tasks WHERE queue=$1`, paperQueueName).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	const topic = fill_event.PaperTopic
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	producer := kafkaClient(t, kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.RequiredAcks(kgo.AllISRAcks()))
	consumer := kafkaClient(t, kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{
		topic: {0: kgo.NewOffset().AtStart()},
	}))
	relayCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- reopened.RelayPaper(relayCtx, producer) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	fetches := consumer.PollRecords(ctx, 1)
	if err := fetches.Err(); err != nil {
		t.Fatal(err)
	}
	records := fetches.Records()
	if len(records) != 1 || string(records[0].Key) != event.ID {
		t.Fatalf("Kafka did not receive the one paper fill")
	}
	decoded, err := fill_event.DecodePaper(records[0].Value)
	if err != nil || decoded != event {
		t.Fatalf("published fill differs from ledger fact: %+v, %v", decoded, err)
	}
	for {
		task, err := reopened.queue.Get(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == pgqueue.TaskStatusCompleted {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("paper task was not completed after Kafka ACK")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pgqueue_tasks`).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("reopened relay changed task count: tasks=%d err=%v", tasks, err)
	}
}
