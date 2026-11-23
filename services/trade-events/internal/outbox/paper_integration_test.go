package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/goware/pgqueue"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"tradingbot/trade-events/internal/fill_event"
)

func TestPaperLedgerHandoffAndKafkaAcknowledgement(t *testing.T) {
	ctx, dsn, db := testPostgres(t)
	data, err := os.ReadFile("../../../../contracts/valid-paper-outbox-fill.json")
	if err != nil {
		t.Fatal(err)
	}
	var entry paperOutboxEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	event, err := fill_event.DecodePaper(entry.Payload)
	if err != nil {
		t.Fatal(err)
	}
	box, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	var mu sync.Mutex
	var available, ackBroken, acknowledged bool
	source := testPaperSource(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /internal/v1/paper/outbox/fills/next":
			if !available || acknowledged {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(entry)
		case "POST /internal/v1/paper/outbox/fills/" + event.AccountID + "/" + event.FillEventID + "/ack":
			if ackBroken {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			acknowledged = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if queued, err := box.queueOnePaper(ctx, source); err != nil || queued {
		t.Fatalf("empty outbox queued: queued=%v err=%v", queued, err)
	}
	mu.Lock()
	available, ackBroken = true, true
	mu.Unlock()
	if queued, err := box.queueOnePaper(ctx, source); !errors.Is(err, errPaperSourceUnavailable) || queued {
		t.Fatalf("lost acknowledgement was reported as completed: queued=%v err=%v", queued, err)
	}
	var tasks int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pgqueue_tasks`).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("HTTP outage lost the durable queue task: tasks=%d err=%v", tasks, err)
	}

	// A fresh relay must publish the durable task even while the account endpoint is down.
	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var taskID int64
	var queuedPayload []byte
	if err := db.QueryRow(ctx, `SELECT id,payload FROM pgqueue_tasks WHERE queue=$1`, paperQueueName).Scan(&taskID, &queuedPayload); err != nil {
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
	go func() { done <- reopened.RelayPaper(relayCtx, producer, source) }()
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
		t.Fatal("Kafka did not receive the one paper fill")
	}
	if !bytes.Equal(records[0].Value, queuedPayload) {
		t.Fatal("published paper fill differs from the queued bytes")
	}
	decoded, err := fill_event.DecodePaper(records[0].Value)
	if err != nil || decoded != event {
		t.Fatalf("published fill differs from ledger fact: %+v, %v", decoded, err)
	}
	mu.Lock()
	ackBroken = false
	mu.Unlock()
	for {
		task, err := reopened.queue.Get(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		acked := acknowledged
		mu.Unlock()
		if acked && task.Status == pgqueue.TaskStatusCompleted {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("paper task was not acknowledged after recovery and Kafka ACK")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pgqueue_tasks`).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("reopened relay changed task count: tasks=%d err=%v", tasks, err)
	}
}

func testPaperSource(t *testing.T, handler http.HandlerFunc) *PaperSource {
	t.Helper()
	const token = "test-account-outbox-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("paper source did not authenticate its request")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	source, err := NewPaperSource(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(source.Close)
	return source
}

func TestPaperSourceRejectsRedirects(t *testing.T) {
	source := testPaperSource(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://invalid.example/paper")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	if _, err := source.next(t.Context(), "fills"); err == nil || errors.Is(err, errPaperSourceUnavailable) {
		t.Fatalf("non-successful redirect: %v", err)
	}
}
