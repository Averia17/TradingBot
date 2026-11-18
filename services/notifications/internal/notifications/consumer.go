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
	id, message, err := decodeMessage(record)
	if err != nil {
		return fmt.Errorf("Kafka record %s: %w", position, err)
	}
	if string(record.Key) != id {
		return fmt.Errorf("Kafka record %s key does not match the fill ID", position)
	}
	if err := send(ctx, message); err != nil {
		return fmt.Errorf("send trade %s from Kafka record %s: %w", id, position, err)
	}

	// Finish the offset commit even if shutdown arrives after Telegram accepted the message.
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()
	if err := kafka.CommitRecords(commit, record); err != nil {
		return fmt.Errorf("Telegram accepted trade %s but Kafka record %s was not confirmed committed: %w", id, position, err)
	}
	slog.Info("trade notification delivered", "fill_id", id, "topic", record.Topic,
		"partition", record.Partition, "offset", record.Offset)
	return nil
}

func decodeMessage(record *kgo.Record) (id, message string, err error) {
	if record.Topic == fill_event.PaperTopic {
		event, err := fill_event.DecodePaper(record.Value)
		if err != nil {
			return "", "", fmt.Errorf("invalid paper fill event: %w", err)
		}
		return event.ID, paperMessage(event), nil
	}
	event, err := fill_event.Decode(record.Value)
	if err != nil {
		return "", "", fmt.Errorf("invalid trade event: %w", err)
	}
	return event.ID, tradeMessage(event), nil
}

func paperMessage(event fill_event.PaperEvent) string {
	side := "Покупка"
	if event.Side == "SELL" {
		side = "Продажа"
	}
	quantity := event.QuantityRaw
	if event.Decimals > 0 {
		if len(quantity) <= event.Decimals {
			quantity = strings.Repeat("0", event.Decimals+1-len(quantity)) + quantity
		}
		point := len(quantity) - event.Decimals
		quantity = quantity[:point] + "." + quantity[point:]
	}
	return fmt.Sprintf("[PAPER · СИМУЛЯЦИЯ] %s %s\nКоличество: %s\nВаловая сумма: %s USD\nКомиссия: %s USD\nИсполнено: %s\nID исполнения: %s",
		side, event.InstrumentID, quantity, event.GrossUSD, event.FeeUSD, event.FilledAt.Format(time.RFC3339Nano), event.ID)
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
