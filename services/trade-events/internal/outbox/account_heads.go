package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

type paperAccountHead struct {
	Version        string    `json:"version"`
	AccountID      string    `json:"account_id"`
	AccountVersion int64     `json:"account_version"`
	ObservedAt     time.Time `json:"observed_at"`
}

func (h paperAccountHead) valid() bool {
	return h.Version == "1.0" && paperAccountIDPattern.MatchString(h.AccountID) &&
		h.AccountVersion > 0 && !h.ObservedAt.IsZero()
}

func publishPaperAccountHeads(ctx context.Context, producer *kgo.Client, source *PaperSource, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := publishPaperAccountHeadBatch(ctx, producer, source); err != nil && ctx.Err() == nil {
			slog.Warn("paper account heads unavailable", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func publishPaperAccountHeadBatch(ctx context.Context, producer *kgo.Client, source *PaperSource) error {
	var after string
	for {
		heads, err := source.heads(ctx, after)
		if err != nil {
			return err
		}
		if len(heads) == 0 {
			return nil
		}
		if len(heads) > paperOutboxPageSize {
			return errors.New("oversized paper account head page")
		}
		records := make([]*kgo.Record, 0, len(heads))
		for _, head := range heads {
			if !head.valid() || head.AccountID <= after {
				return errors.New("invalid paper account head response")
			}
			payload, err := json.Marshal(head)
			if err != nil {
				return errors.New("cannot encode paper account head")
			}
			records = append(records, &kgo.Record{Topic: PaperAccountHeadTopic, Key: []byte(head.AccountID), Value: payload})
			after = head.AccountID
		}
		publish, cancel := context.WithTimeout(ctx, 12*time.Second)
		err = producer.ProduceSync(publish, records...).FirstErr()
		cancel()
		if err != nil || len(heads) < paperOutboxPageSize {
			return err
		}
	}
}
