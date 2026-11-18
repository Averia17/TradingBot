package analytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Consume acknowledges Kafka only after the record and any resulting state
// are durable. A gap can be staged and acknowledged before its predecessor.
func Consume(ctx context.Context, store *Store, brokers []string, group, accountID string, ready func()) error {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumeTopics(AccountTopic, HeadTopic, ValuationTopic),
		kgo.ConsumerGroup(group),
		kgo.ConsumeResetOffset(kgo.NoResetOffset().AtStart()),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
	)
	if err != nil {
		return fmt.Errorf("configure analytics Kafka consumer: %w", err)
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
		defer cancel()
		_ = client.LeaveGroupContext(shutdown)
		client.Close()
	}()
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = client.Ping(ping)
	cancel()
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("check analytics Kafka connection: %w", err)
	}
	for ctx.Err() == nil {
		if err := consumeRecord(ctx, client, store, accountID, ready); err != nil {
			return err
		}
	}
	return nil
}

func consumeRecord(ctx context.Context, client *kgo.Client, store *Store, accountID string, ready func()) error {
	fetches := client.PollRecords(ctx, 1)
	defer client.AllowRebalance()
	if ctx.Err() != nil {
		return nil
	}
	if err := fetches.Err(); err != nil {
		return fmt.Errorf("fetch analytics record: %w", err)
	}
	iterator := fetches.RecordIter()
	if iterator.Done() {
		return nil
	}
	record := iterator.Next()
	position := fmt.Sprintf("%s[%d]@%d", record.Topic, record.Partition, record.Offset)
	key := string(record.Key)
	if !ValidAccountID(key) {
		return fmt.Errorf("invalid account key at %s", position)
	}
	if key == accountID {
		apply, cancel := context.WithTimeout(ctx, 8*time.Second)
		err := ingest(apply, store, accountID, record.Topic, record.Value)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("apply Kafka record %s: %w", position, err)
		}
	}
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := client.CommitRecords(commit, record); err != nil && ctx.Err() == nil {
		return fmt.Errorf("commit analytics Kafka offset at %s: %w", position, err)
	}
	if ready != nil && key == accountID && ctx.Err() == nil {
		ready()
	}
	return nil
}

func ingest(ctx context.Context, store *Store, accountID, topic string, value []byte) error {
	switch topic {
	case AccountTopic:
		event, err := DecodeAccount(value)
		return ingestAccountRecord(event.AccountID, accountID, err, func() error { return store.account(ctx, event) })
	case HeadTopic:
		head, err := DecodeHead(value)
		return ingestAccountRecord(head.AccountID, accountID, err, func() error { return store.head(ctx, head) })
	case ValuationTopic:
		row, err := DecodeValuation(value)
		return ingestAccountRecord(row.AccountID, accountID, err, func() error { return store.valuation(ctx, row) })
	default:
		return errors.New("unknown analytics topic")
	}
}

func ingestAccountRecord(incoming, expected string, err error, apply func() error) error {
	if err != nil {
		return err
	}
	if incoming != expected {
		return errors.New("analytics payload account differs from the Kafka key")
	}
	return apply()
}

// PublishArchive validates an entire immutable JSON array before publishing.
// A retry is safe because record IDs and payload digests are stable.
func PublishArchive(ctx context.Context, brokers []string, accountID, path string) (int, string, error) {
	const maxArchiveBytes = 16 * 1024 * 1024
	file, err := os.Open(path)
	if err != nil {
		return 0, "", errors.New("could not open valuation archive")
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxArchiveBytes+1))
	if err != nil || len(body) > maxArchiveBytes {
		return 0, "", errors.New("could not read valuation archive within size limit")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(body, &entries); err != nil || len(entries) == 0 || len(entries) > 10_000 {
		return 0, "", errors.New("valuation archive must be an array of 1 to 10000 records")
	}
	ids := make(map[string]bool, len(entries))
	for i, data := range entries {
		row, err := DecodeValuation(data)
		if err != nil || row.AccountID != accountID || ids[row.ID] {
			return 0, "", fmt.Errorf("invalid or repeated valuation record at index %d", i)
		}
		ids[row.ID] = true
	}
	client, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.RecordDeliveryTimeout(20*time.Second))
	if err != nil {
		return 0, "", fmt.Errorf("configure valuation Kafka publisher: %w", err)
	}
	defer client.Close()
	for i, data := range entries {
		if err := client.ProduceSync(ctx, &kgo.Record{Topic: ValuationTopic, Key: []byte(accountID), Value: data}).FirstErr(); err != nil {
			return i, "", fmt.Errorf("publish valuation record %d: %w", i, err)
		}
	}
	digest := sha256.Sum256(body)
	return len(entries), hex.EncodeToString(digest[:]), nil
}
