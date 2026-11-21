package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kgo"
)

var paperAccountIDPattern = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

const paperAccountReplayPageSize = 128

// ReplayPaperAccount republishes a bounded ledger prefix without changing its
// pgqueue state. Consumers deduplicate overlap with concurrent delivery.
func (o *Outbox) ReplayPaperAccount(ctx context.Context, producer *kgo.Client, accountID string) (int64, error) {
	if !paperAccountIDPattern.MatchString(accountID) {
		return 0, errors.New("-replay-paper-account requires a canonical account UUID")
	}
	var version int64
	err := o.db.Conn.QueryRow(ctx, `SELECT account_version FROM paper_account_analytics_heads WHERE account_id=$1`, accountID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("paper account head not found")
	}
	if err != nil {
		return 0, errors.New("cannot read paper account replay head")
	}
	var replayed int64
	for replayed < version {
		batch, err := o.readPaperAccountReplayBatch(ctx, accountID, replayed, version)
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

func (o *Outbox) readPaperAccountReplayBatch(ctx context.Context, accountID string, after, through int64) ([]*kgo.Record, error) {
	rows, err := o.db.Conn.Query(ctx, `SELECT account_version,event_id::text,payload FROM paper_account_outbox
		WHERE account_id=$1 AND account_version>$2 AND account_version<=$3 ORDER BY account_version LIMIT 128`, accountID, after, through)
	if err != nil {
		return nil, fmt.Errorf("read paper account replay page after version %d: %w", after, err)
	}
	defer rows.Close()
	records := make([]*kgo.Record, 0, min(paperAccountReplayPageSize, through-after))
	kafkaKey := []byte(accountID)
	want := after
	for rows.Next() {
		want++
		var version int64
		var eventID string
		var payload []byte
		if err := rows.Scan(&version, &eventID, &payload); err != nil {
			return nil, fmt.Errorf("read paper account replay version %d: %w", want, err)
		}
		event, err := decodePaperAccountEvent(payload)
		if err != nil || event.AccountID != accountID || version != want || event.AccountVersion != want || event.EventID != eventID {
			return nil, fmt.Errorf("inconsistent paper account replay version %d", want)
		}
		payload, err = json.Marshal(json.RawMessage(payload))
		if err != nil {
			return nil, fmt.Errorf("encode paper account replay version %d: %w", want, err)
		}
		records = append(records, &kgo.Record{Topic: PaperAccountTopic, Key: kafkaKey, Value: payload})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read paper account replay page after version %d: %w", after, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("no paper account replay event after version %d", after)
	}
	return records, nil
}
