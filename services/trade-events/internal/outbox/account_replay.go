package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

var paperAccountIDPattern = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

// ReplayPaperAccount republishes a bounded history prefix without changing delivery state.
func ReplayPaperAccount(ctx context.Context, producer *kgo.Client, source *PaperSource, accountID string) (int64, error) {
	if !paperAccountIDPattern.MatchString(accountID) {
		return 0, errors.New("-replay-paper-account requires a canonical account UUID")
	}
	head, err := source.head(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if !head.valid() || head.AccountID != accountID {
		return 0, errors.New("invalid paper account replay head")
	}
	var replayed int64
	for replayed < head.AccountVersion {
		batch, err := readPaperAccountReplayBatch(ctx, source, accountID, replayed, head.AccountVersion)
		if err != nil {
			return replayed, err
		}
		publish, cancel := context.WithTimeout(ctx, 12*time.Second)
		err = producer.ProduceSync(publish, batch...).FirstErr()
		cancel()
		if err != nil {
			return replayed, fmt.Errorf("publish paper account replay after version %d: %w", replayed, err)
		}
		replayed += int64(len(batch))
	}
	return replayed, nil
}

func readPaperAccountReplayBatch(ctx context.Context, source *PaperSource, accountID string, after, through int64) ([]*kgo.Record, error) {
	entries, err := source.accountHistory(ctx, accountID, after, through)
	if err != nil {
		return nil, fmt.Errorf("read paper account replay page after version %d: %w", after, err)
	}
	if len(entries) == 0 || int64(len(entries)) > min(paperOutboxPageSize, through-after) {
		return nil, fmt.Errorf("invalid paper account replay page after version %d", after)
	}
	records := make([]*kgo.Record, 0, len(entries))
	kafkaKey := []byte(accountID)
	for i, entry := range entries {
		want := after + int64(i) + 1
		event, err := decodePaperAccountEvent(entry.Payload)
		if err != nil || event.AccountID != accountID || entry.AccountID != accountID ||
			event.AccountVersion != want || entry.AccountVersion != want || event.EventID != entry.EventID {
			return nil, fmt.Errorf("inconsistent paper account replay version %d", want)
		}
		payload, err := json.Marshal(entry.Payload)
		if err != nil {
			return nil, fmt.Errorf("encode paper account replay version %d: %w", want, err)
		}
		records = append(records, &kgo.Record{Topic: PaperAccountTopic, Key: kafkaKey, Value: payload})
	}
	return records, nil
}
