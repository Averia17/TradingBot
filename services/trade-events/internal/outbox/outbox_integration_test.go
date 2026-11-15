package outbox

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goware/pgqueue"
	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"tradingbot/trade-events/internal/fill_event"
)

func TestCommittedFillOutboxPublishesToKafka(t *testing.T) {
	ctx, dsn, db := testPostgres(t)
	outbox, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	if _, err := db.Exec(ctx, `CREATE TABLE test_fills (id text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../../../contracts/valid-trade-event.json")
	if err != nil {
		t.Fatal(err)
	}
	event, err := fill_event.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	writeFill := func(commit bool) int64 {
		t.Helper()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "INSERT INTO test_fills (id) VALUES ($1)", event.ID); err != nil {
			t.Fatal(err)
		}
		id, err := outbox.EnqueueTx(ctx, tx, event)
		if err != nil {
			t.Fatal(err)
		}
		if commit {
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	writeFill(false)
	assertCounts(t, ctx, db, 0)
	id := writeFill(true)
	assertCounts(t, ctx, db, 1)

	// Use a second PostgreSQL connection to exercise the same queue after a restart.
	reader, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	topic := fill_event.DefaultTopic + "." + strings.ToLower(rand.Text())
	brokers := testBrokers(t, topic)
	producer := kafkaClient(t, kgo.SeedBrokers(brokers...), kgo.AllowAutoTopicCreation(), kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.RecordDeliveryTimeout(10*time.Second))
	consumer := kafkaClient(t, kgo.SeedBrokers(brokers...), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{
		topic: {0: kgo.NewOffset().AtStart()},
	}))
	relayCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- reader.Relay(relayCtx, producer, topic) }()
	defer func() {
		stop()
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
		t.Fatal("Kafka must receive the confirmed fill keyed by its event ID")
	}
	published, err := fill_event.Decode(records[0].Value)
	if err != nil || published != event {
		t.Fatalf("published event differs from confirmed fill: %v", err)
	}
	for {
		task, err := reader.queue.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == pgqueue.TaskStatusCompleted {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("pgqueue task was not completed after Kafka ACK")
		case <-time.After(25 * time.Millisecond):
		}
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if replay, err := outbox.EnqueueTx(ctx, tx, event); err != nil || replay != id {
		t.Fatalf("identical replay task ID = %d, error = %v", replay, err)
	}
	event.Quantity = "1234567.00001"
	if _, err := outbox.EnqueueTx(ctx, tx, event); !errors.Is(err, ErrConflict) {
		t.Fatalf("modified replay error = %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := outbox.Requeue(ctx, id); !errors.Is(err, pgqueue.ErrTaskNotRecoverable) {
		t.Fatalf("completed task must not be requeued, got %v", err)
	}
	assertCounts(t, ctx, db, 1)
}

func testPostgres(t *testing.T) (context.Context, string, *pgx.Conn) {
	t.Helper()
	base := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	dsn, err := url.Parse(base)
	if err != nil || dsn.Scheme == "" {
		t.Fatal("test database DSN must be a PostgreSQL URI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "trade_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer admin.Close(cleanupCtx)
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	params := dsn.Query()
	params.Set("search_path", schema)
	dsn.RawQuery = params.Encode()
	db, err := pgx.Connect(ctx, dsn.String())
	if err != nil {
		t.Fatal("connect to test schema failed")
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return ctx, dsn.String(), db
}

func assertCounts(t *testing.T, ctx context.Context, db *pgx.Conn, want int) {
	t.Helper()
	var fills, tasks int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM test_fills),
		(SELECT count(*) FROM pgqueue_tasks)`).Scan(&fills, &tasks); err != nil {
		t.Fatal(err)
	}
	if fills != want || tasks != want {
		t.Fatalf("fill and outbox counts = %d, %d; want %d each", fills, tasks, want)
	}
}

func testBrokers(t *testing.T, topic string) []string {
	t.Helper()
	if brokers := os.Getenv("TRADINGBOT_TEST_KAFKA_BROKERS"); brokers != "" {
		return strings.Split(brokers, ",")
	}
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	return cluster.ListenAddrs()
}

func kafkaClient(t *testing.T, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	client, err := kgo.NewClient(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}
