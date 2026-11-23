package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goware/pgqueue"
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
	var published, acknowledged atomic.Bool
	var headVersion atomic.Int64
	headVersion.Store(event.AccountVersion)
	entry := paperOutboxEntry{AccountID: event.AccountID, EventID: event.EventID, AccountVersion: event.AccountVersion, Payload: data}
	source := testPaperSource(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		head := paperAccountHead{Version: "1.0", AccountID: event.AccountID, AccountVersion: headVersion.Load(), ObservedAt: time.Now().UTC()}
		switch r.Method + " " + r.URL.Path {
		case "GET /internal/v1/paper/outbox/accounts/next":
			if !published.Load() || acknowledged.Load() {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(entry)
		case "POST /internal/v1/paper/outbox/accounts/" + event.AccountID + "/1/" + event.EventID + "/ack":
			acknowledged.Store(true)
			w.WriteHeader(http.StatusNoContent)
		case "GET /internal/v1/paper/outbox/heads":
			_ = json.NewEncoder(w).Encode([]paperAccountHead{head})
		case "GET /internal/v1/paper/outbox/heads/" + event.AccountID:
			_ = json.NewEncoder(w).Encode(head)
		case "GET /internal/v1/paper/outbox/accounts/" + event.AccountID:
			entries := []paperOutboxEntry{}
			if r.URL.Query().Get("after") == "0" {
				entries = append(entries, entry)
			}
			_ = json.NewEncoder(w).Encode(entries)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	box, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	if queued, err := box.queueOnePaperAccount(ctx, source); err != nil || queued {
		t.Fatalf("empty outbox queued: queued=%v err=%v", queued, err)
	}
	published.Store(true)
	if queued, err := box.queueOnePaperAccount(ctx, source); err != nil || !queued || !acknowledged.Load() {
		t.Fatalf("committed event was not queued and acknowledged: queued=%v err=%v", queued, err)
	}
	if queued, err := box.queueOnePaperAccount(ctx, source); err != nil || queued {
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
	go func() { done <- reopened.RelayPaperAccounts(relayCtx, producer, source, time.Minute) }()
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
	if replayed, err := ReplayPaperAccount(ctx, producer, source, event.AccountID); err != nil || replayed != 1 {
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
	headVersion.Store(2)
	if _, err := ReplayPaperAccount(ctx, producer, source, event.AccountID); err == nil {
		t.Fatal("replay accepted a missing source version")
	}
}
