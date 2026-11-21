package outbox

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/goware/pgqueue"
	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/twmb/franz-go/pkg/kgo"
	"golang.org/x/sync/errgroup"
)

const (
	PaperAccountTopic     = "tradingbot.portfolio.account.paper.v1"
	PaperAccountHeadTopic = "tradingbot.portfolio.account-head.paper.v1"
	paperAccountQueue     = "paper-account-analytics-v1"
)

//go:embed paper-account-event.schema.json
var paperAccountSchemaJSON []byte

var paperAccountSchema = compilePaperAccountSchema()

type paperAccountEvent struct {
	AccountID      string `json:"account_id"`
	EventID        string `json:"event_id"`
	AccountVersion int64  `json:"account_version"`
}

func compilePaperAccountSchema() *jsonschema.Schema {
	const name = "https://tradingbot.local/contracts/paper-account-event.schema.json"
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(paperAccountSchemaJSON))
	if err != nil {
		panic(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(name, value); err != nil {
		panic(err)
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		panic(err)
	}
	return schema
}

func decodePaperAccountEvent(payload []byte) (paperAccountEvent, error) {
	var event paperAccountEvent
	if len(payload) == 0 || len(payload) > 16<<10 || !utf8.Valid(payload) {
		return event, errors.New("invalid paper account event JSON")
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil || paperAccountSchema.Validate(value) != nil {
		return event, errors.New("invalid paper account event schema")
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return event, errors.New("invalid paper account event identity")
	}
	return event, nil
}

type paperAccountTask struct {
	Key     string          `json:"key"`
	Payload json.RawMessage `json:"payload"`
}

func (e paperAccountEvent) taskKey() string {
	return e.AccountID + ":" + strconv.FormatInt(e.AccountVersion, 10)
}

var publishPaperAccountJob = publishJobSpec[paperAccountTask](paperAccountQueue)

func (o *Outbox) RelayPaperAccounts(ctx context.Context, producer *kgo.Client, headInterval time.Duration) error {
	if headInterval <= 0 {
		return errors.New("paper account head interval must be positive")
	}
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := o.db.Conn.Exec(startup, `SELECT account_id,account_version,event_id,payload,created_at,queued_at FROM paper_account_outbox LIMIT 0`); err != nil {
		return errors.New("paper account outbox is not accessible in the configured PostgreSQL schema")
	}
	if _, err := o.db.Conn.Exec(startup, `SELECT account_id,account_version FROM paper_account_analytics_heads LIMIT 0`); err != nil {
		return errors.New("paper account heads are not accessible in the configured PostgreSQL schema")
	}
	worker := pgqueue.NewWorker(o.queue, pgqueue.WithReapInterval(5*time.Second))
	handler := &paperAccountRelay{relay{producer: producer, topic: PaperAccountTopic}}
	if err := pgqueue.Register(worker, publishPaperAccountJob, handler); err != nil {
		return err
	}
	cancel()
	group, runCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return runHandoff(runCtx, worker, "paper account", o.queueOnePaperAccount) })
	group.Go(func() error {
		o.publishPaperAccountHeads(runCtx, producer, headInterval)
		return nil
	})
	return group.Wait()
}

func (o *Outbox) queueOnePaperAccount(ctx context.Context) (bool, error) {
	return o.handoff(ctx, "paper account", o.movePaperAccountEvent)
}

func (o *Outbox) movePaperAccountEvent(ctx context.Context, tx pgx.Tx) (bool, error) {
	var accountID, eventID string
	var version int64
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT account_id::text,account_version,event_id::text,payload FROM paper_account_outbox
		WHERE queued_at IS NULL ORDER BY created_at,account_id,account_version
		LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&accountID, &version, &eventID, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select pending paper account event: %w", err)
	}
	event, err := decodePaperAccountEvent(payload)
	if err != nil || event.AccountID != accountID || event.AccountVersion != version || event.EventID != eventID {
		return false, fmt.Errorf("invalid paper account outbox event for account %s version %d", accountID, version)
	}
	task := paperAccountTask{Key: event.taskKey(), Payload: payload}
	queuedPayload, err := json.Marshal(task)
	if err != nil {
		return false, fmt.Errorf("encode paper account outbox task: %w", err)
	}
	if _, err := enqueueExactTask(ctx, tx, paperAccountQueue, task.Key, queuedPayload); err != nil {
		return false, err
	}
	mark, err := tx.Exec(ctx, `UPDATE paper_account_outbox SET queued_at=clock_timestamp() WHERE account_id=$1 AND account_version=$2`, accountID, version)
	if err != nil {
		return false, fmt.Errorf("mark paper account event queued: %w", err)
	}
	if mark.RowsAffected() != 1 {
		return false, errors.New("paper account handoff did not update exactly one outbox row")
	}
	return true, nil
}

type paperAccountRelay struct{ relay }

func (r *paperAccountRelay) RunTask(ctx context.Context, job *pgqueue.Job[paperAccountTask]) pgqueue.Result {
	event, err := decodePaperAccountEvent(job.Payload.Payload)
	if err != nil || job.Payload.Key != event.taskKey() {
		return pgqueue.Fail(errors.New("invalid queued paper account event"))
	}
	return r.publish(ctx, job.Task.ID, event.EventID, event.AccountID, job.Payload.Payload)
}
