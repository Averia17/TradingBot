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

func (o *Outbox) RelayPaperAccounts(ctx context.Context, producer *kgo.Client, source *PaperSource, headInterval time.Duration) error {
	if headInterval <= 0 {
		return errors.New("paper account head interval must be positive")
	}
	worker := pgqueue.NewWorker(o.queue, pgqueue.WithReapInterval(5*time.Second))
	handler := &paperAccountRelay{relay{producer: producer, topic: PaperAccountTopic}}
	if err := pgqueue.Register(worker, publishPaperAccountJob, handler); err != nil {
		return err
	}
	group, runCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return runHandoff(runCtx, worker, "paper account", func(ctx context.Context) (bool, error) {
			return o.queueOnePaperAccount(ctx, source)
		})
	})
	group.Go(func() error {
		publishPaperAccountHeads(runCtx, producer, source, headInterval)
		return nil
	})
	return group.Wait()
}

func (o *Outbox) queueOnePaperAccount(ctx context.Context, source *PaperSource) (bool, error) {
	entry, err := source.next(ctx, "accounts")
	if err != nil || entry == nil {
		return false, err
	}
	event, err := decodePaperAccountEvent(entry.Payload)
	if err != nil || event.AccountID != entry.AccountID || event.AccountVersion != entry.AccountVersion || event.EventID != entry.EventID {
		return false, errors.New("invalid paper account outbox event identity")
	}
	task := paperAccountTask{Key: event.taskKey(), Payload: entry.Payload}
	queuedPayload, err := json.Marshal(task)
	if err != nil {
		return false, fmt.Errorf("encode paper account outbox task: %w", err)
	}
	if err := o.handoff(ctx, paperAccountQueue, task.Key, queuedPayload); err != nil {
		return false, err
	}
	path := "/accounts/" + event.AccountID + "/" + strconv.FormatInt(event.AccountVersion, 10) + "/" + event.EventID
	if err := source.ack(ctx, path); err != nil {
		return false, err
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
