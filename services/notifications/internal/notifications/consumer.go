package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"tradingbot/notifications/internal/fill_event"
)

// Consume sends one Kafka record at a time and acknowledges only confirmed deliveries.
func Consume(ctx context.Context, brokers []string, topic, group string, send func(context.Context, string) error) error {
	kafka, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumeTopics(topic),
		kgo.ConsumerGroup(group),
		kgo.ConsumeResetOffset(kgo.NoResetOffset().AtStart()),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
	)
	if err != nil {
		return fmt.Errorf("configure notification consumer: %w", err)
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = kafka.LeaveGroupContext(shutdown)
		kafka.Close()
	}()

	slog.Info("polling Kafka for trade notifications", "topic", topic, "group", group)
	for ctx.Err() == nil {
		if err := consumeOne(ctx, kafka, send); err != nil {
			return err
		}
	}
	return nil
}

func consumeOne(ctx context.Context, kafka *kgo.Client, send func(context.Context, string) error) error {
	fetches := kafka.PollRecords(ctx, 1)
	defer kafka.AllowRebalance()
	if ctx.Err() != nil {
		return nil
	}
	if err := fetches.Err(); err != nil {
		return fmt.Errorf("fetch trade notification: %w", err)
	}
	records := fetches.RecordIter()
	if records.Done() {
		return nil
	}
	record := records.Next()
	position := fmt.Sprintf("%s[%d]@%d", record.Topic, record.Partition, record.Offset)
	event, err := fill_event.Decode(record.Value)
	if err != nil {
		return fmt.Errorf("Kafka record %s contains an invalid trade event", position)
	}
	if string(record.Key) != event.ID {
		return fmt.Errorf("Kafka record %s key does not match the fill ID", position)
	}
	if err := send(ctx, tradeMessage(event)); err != nil {
		return fmt.Errorf("send trade %s from Kafka record %s: %w", event.ID, position, err)
	}

	// Finish the offset commit even if shutdown arrives after Telegram accepted the message.
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()
	if err := kafka.CommitRecords(commit, record); err != nil {
		return fmt.Errorf("Telegram accepted trade %s but Kafka record %s was not confirmed committed: %w", event.ID, position, err)
	}
	slog.Info("trade notification delivered", "fill_id", event.ID, "topic", record.Topic,
		"partition", record.Partition, "offset", record.Offset)
	return nil
}

func tradeMessage(event fill_event.Event) string {
	mode := strings.ToUpper(event.Mode)
	if event.Source == fill_event.SourceExchangeFixture {
		mode += " · ТЕСТ"
	}
	side := "Покупка"
	if event.Side == "SELL" {
		side = "Продажа"
	}
	return fmt.Sprintf("[%s] %s %s\nКоличество: %s\nЦена: %s %s\nИсполнено: %s\nID исполнения: %s",
		mode, side, event.InstrumentID, event.Quantity,
		event.Price, event.Currency, event.FilledAt.UTC().Format(time.RFC3339Nano), event.ID)
}
