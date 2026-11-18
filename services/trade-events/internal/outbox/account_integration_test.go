package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/goware/pgqueue"
	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestPaperAccountRelayPublishesCommittedEventAndHead(t *testing.T) {
	ctx, dsn, db := testPostgres(t)
	data, err := os.ReadFile("../../../../contracts/valid-paper-account-event.json")
	if err != nil {
		t.Fatal(err)
	}
	event, err := decodePaperAccountEvent(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		for _, name := range []string{"001_accounts.sql", "009_account_outbox.sql"} {
			sql, err := os.ReadFile("../../../control/internal/ledger/" + name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql), pgx.QueryExecModeSimpleProtocol); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
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
	const zeroHash = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := tx.Exec(ctx, `INSERT INTO paper_accounts (account_id,creation_hash,cash_usd,version)
		VALUES ($1,decode($3,'hex'),1000,$2)`, event.AccountID, event.AccountVersion, zeroHash); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_events (account_id,event_id,account_version,kind,request_hash,payload)
		VALUES ($1,$2,$3,'paper_account.created',decode($4,'hex'),'{}')`, event.AccountID, event.EventID, event.AccountVersion, zeroHash); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_account_outbox (account_id,account_version,event_id,payload,created_at)
		VALUES ($1,$2,$3,$4,clock_timestamp())`, event.AccountID, event.AccountVersion, event.EventID, data); err != nil {
		t.Fatal(err)
	}
	if queued, err := box.queueOnePaperAccount(ctx); err != nil || queued {
		t.Fatalf("uncommitted event was moved to the relay: queued=%v err=%v", queued, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if queued, err := box.queueOnePaperAccount(ctx); err != nil || !queued {
		t.Fatalf("committed event was not queued: queued=%v err=%v", queued, err)
	}
	if queued, err := box.queueOnePaperAccount(ctx); err != nil || queued {
		t.Fatalf("committed event handed off twice: queued=%v err=%v", queued, err)
	}

	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var taskID int64
	if err := db.QueryRow(ctx, `SELECT id FROM pgqueue_tasks WHERE queue=$1`, paperAccountQueue).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, PaperAccountTopic, PaperAccountHeadTopic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	producer := kafkaClient(t, kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.RequiredAcks(kgo.AllISRAcks()))
	consumer := kafkaClient(t, kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{
		PaperAccountTopic:     {0: kgo.NewOffset().AtStart()},
		PaperAccountHeadTopic: {0: kgo.NewOffset().AtStart()},
	}))
	relayCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- reopened.RelayPaperAccounts(relayCtx, producer, time.Minute) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()

	seen := make(map[string]bool)
	var publishedBytes []byte
	for len(seen) != 2 {
		fetches := consumer.PollRecords(ctx, 2)
		if err := fetches.Err(); err != nil {
			t.Fatal(err)
		}
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		for _, record := range fetches.Records() {
			if string(record.Key) != event.AccountID {
				t.Fatalf("%s used Kafka key %s, want %s", record.Topic, record.Key, event.AccountID)
			}
			switch record.Topic {
			case PaperAccountTopic:
				publishedBytes = record.Value
				published, err := decodePaperAccountEvent(record.Value)
				if err != nil || published != event {
					t.Fatalf("published account event differs from the ledger fact: %+v, %v", published, err)
				}
			case PaperAccountHeadTopic:
				var head paperAccountHead
				err := json.Unmarshal(record.Value, &head)
				if err != nil || head.Version != "1.0" || head.AccountID != event.AccountID || head.AccountVersion != event.AccountVersion || head.ObservedAt.IsZero() {
					t.Fatalf("invalid published head: %+v, %v", head, err)
				}
			}
			seen[record.Topic] = true
		}
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
			t.Fatal("account event was not completed after Kafka ACK")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if replayed, err := reopened.ReplayPaperAccount(ctx, producer, event.AccountID); err != nil || replayed != 1 {
		t.Fatalf("replay failed: count=%d err=%v", replayed, err)
	}
	fetches := consumer.PollRecords(ctx, 1)
	if err := fetches.Err(); err != nil {
		t.Fatal(err)
	}
	records := fetches.Records()
	if len(records) != 1 || records[0].Topic != PaperAccountTopic || !bytes.Equal(records[0].Value, publishedBytes) {
		t.Fatal("replay bytes differ from the normal account event")
	}
	if _, err := db.Exec(ctx, `UPDATE paper_accounts SET version=2 WHERE account_id=$1`, event.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.ReplayPaperAccount(ctx, producer, event.AccountID); err == nil {
		t.Fatal("replay accepted a missing source version")
	}
}
