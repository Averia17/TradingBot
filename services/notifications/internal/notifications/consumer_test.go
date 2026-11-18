package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"tradingbot/notifications/internal/fill_event"
)

func TestConsumerCommitsOnlyConfirmedRecords(t *testing.T) {
	const topic, group = "trade-test", "notification-test"
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	producer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(producer.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	data, err := os.ReadFile("../../../../contracts/valid-trade-event.json")
	if err != nil {
		t.Fatal(err)
	}
	first, err := fill_event.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = "test-second-fill"
	second.Source = fill_event.SourceExchangeFixture
	secondData, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.ProduceSync(ctx,
		&kgo.Record{Topic: topic, Key: []byte(first.ID), Value: data},
		&kgo.Record{Topic: topic, Key: []byte(second.ID), Value: secondData},
		&kgo.Record{Topic: topic, Key: []byte("wrong-key"), Value: data},
	).FirstErr(); err != nil {
		t.Fatal(err)
	}

	var attempts atomic.Int32
	var failSecond atomic.Bool
	failSecond.Store(true)
	sendTelegram := testTelegram(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := first
		if attempts.Add(1) > 1 {
			expected = second
		}
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("invalid request JSON: %v", err)
		}
		if !strings.Contains(payload.Text, expected.ID) || !strings.Contains(payload.Text, expected.Quantity) || !strings.Contains(payload.Text, expected.Price+" "+expected.Currency) {
			t.Errorf("message changes the fill ID or decimal display: %q", payload.Text)
		}
		wantMarker := expected.Source == fill_event.SourceExchangeFixture
		if gotMarker := strings.Contains(payload.Text, "ТЕСТ"); gotMarker != wantMarker {
			t.Errorf("test marker in message = %v, want %v", gotMarker, wantMarker)
		}
		if failSecond.Load() && strings.Contains(payload.Text, second.ID) {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"ok":false,"error_code":502}`)
			return
		}
		writeTelegramSuccess(w)
	}))

	if err := Consume(ctx, cluster.ListenAddrs(), topic, group, sendTelegram.Send); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("expected Telegram failure, got %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("initial Telegram attempts = %d, want 2", got)
	}

	failSecond.Store(false)
	for _, totalAttempts := range []int32{3, 3} {
		if err := Consume(ctx, cluster.ListenAddrs(), topic, group, sendTelegram.Send); err == nil || !strings.Contains(err.Error(), "key does not match") {
			t.Fatalf("expected uncommitted invalid record, got %v", err)
		}
		if got := attempts.Load(); got != totalAttempts {
			t.Fatalf("Telegram attempts = %d, want %d", got, totalAttempts)
		}
	}
}

func TestConsumerDeliversExactPaperFillOnDedicatedTopic(t *testing.T) {
	const topic, group = fill_event.PaperTopic, "paper-notification-test"
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	producer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(producer.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	data, err := os.ReadFile("../../../../contracts/valid-paper-fill-event.json")
	if err != nil {
		t.Fatal(err)
	}
	event, err := fill_event.DecodePaper(data)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := os.ReadFile("../../../../contracts/valid-trade-event.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.ProduceSync(ctx,
		&kgo.Record{Topic: topic, Key: []byte(event.ID), Value: data},
		&kgo.Record{Topic: topic, Key: []byte(event.ID), Value: legacy},
	).FirstErr(); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	for range 2 {
		err := Consume(ctx, cluster.ListenAddrs(), topic, group, func(_ context.Context, message string) error {
			delivered = append(delivered, message)
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "invalid paper fill event") {
			t.Fatalf("expected wrong-version record to remain uncommitted, got %v", err)
		}
	}
	if len(delivered) != 1 {
		t.Fatalf("successful deliveries = %d, want 1", len(delivered))
	}
	for _, want := range []string{"PAPER · СИМУЛЯЦИЯ", "0.000000000000000003", "1 USD", "0.00000001 USD", event.ID} {
		if !strings.Contains(delivered[0], want) {
			t.Fatalf("delivered message lacks %q: %s", want, delivered[0])
		}
	}
}
