package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kgo"
)

type paperAccountHead struct {
	Version        string    `json:"version"`
	AccountID      string    `json:"account_id"`
	AccountVersion int64     `json:"account_version"`
	ObservedAt     time.Time `json:"observed_at"`
}

func (o *Outbox) readPaperAccountHeads(ctx context.Context) ([]paperAccountHead, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := o.db.Conn.Query(ctx, `SELECT account_id::text,account_version,statement_timestamp()
		FROM paper_account_analytics_heads`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (paperAccountHead, error) {
		head := paperAccountHead{Version: "1.0"}
		err := row.Scan(&head.AccountID, &head.AccountVersion, &head.ObservedAt)
		head.ObservedAt = head.ObservedAt.UTC()
		return head, err
	})
}

func (o *Outbox) publishPaperAccountHeads(ctx context.Context, producer *kgo.Client, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := o.publishPaperAccountHeadBatch(ctx, producer); err != nil && ctx.Err() == nil {
			slog.Warn("paper account heads unavailable", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (o *Outbox) publishPaperAccountHeadBatch(ctx context.Context, producer *kgo.Client) error {
	heads, err := o.readPaperAccountHeads(ctx)
	if err != nil {
		return errors.New("cannot read authoritative paper account heads")
	}
	records := make([]*kgo.Record, 0, len(heads))
	for _, head := range heads {
		payload, err := json.Marshal(head)
		if err != nil {
			return errors.New("cannot encode paper account head")
		}
		records = append(records, &kgo.Record{Topic: PaperAccountHeadTopic, Key: []byte(head.AccountID), Value: payload})
	}
	if len(records) == 0 {
		return nil
	}
	publish, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return producer.ProduceSync(publish, records...).FirstErr()
}
