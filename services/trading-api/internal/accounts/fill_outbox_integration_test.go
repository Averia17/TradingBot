package accounts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPaperFillOutboxRecordsExactPartialAndFullExecutions(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "20")
	req := reserveFixture(1, 1)
	req.ResearchJobID = identifier(69)
	req.Instrument.Decimals = 18
	req.QuantityRaw, req.GrossUSD, req.MaxFeeUSD = "6", "2", "0.00000001"
	order, _, err := store.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	partial := ResolutionRequest{AccountID: req.AccountID, OrderID: req.OrderID, EventID: identifier(51),
		Outcome: "partially_filled", QuantityRaw: "3", GrossUSD: "1", FeeUSD: "0.00000001", ObservedAt: order.CreatedAt}
	if _, created, err := store.Resolve(ctx, partial); err != nil || !created {
		t.Fatalf("partial fill: created=%v err=%v", created, err)
	}
	// A lost response is replayed after a fresh process opens the ledger.
	store.Close()
	store = openLedger(t, ctx, dsn)
	if _, created, err := store.Resolve(ctx, partial); err != nil || created {
		t.Fatalf("partial replay: created=%v err=%v", created, err)
	}
	last := ResolutionRequest{AccountID: req.AccountID, OrderID: req.OrderID, EventID: identifier(52),
		Outcome: "filled", GrossUSD: "1", FeeUSD: "0", ObservedAt: order.CreatedAt}
	if _, created, err := store.Resolve(ctx, last); err != nil || !created {
		t.Fatalf("final fill: created=%v err=%v", created, err)
	}
	schema := paperFillWireSchema(t)
	for _, resolution := range []ResolutionRequest{partial, last} {
		var payload []byte
		var exactRecordedAt bool
		err := store.pool.QueryRow(ctx, `SELECT o.payload, (o.payload->>'recorded_at')::timestamptz=e.recorded_at
			FROM paper_fill_outbox o JOIN paper_events e USING (account_id,event_id)
			WHERE o.account_id=$1 AND o.event_id=$2 AND o.queued_at IS NULL`, req.AccountID, resolution.EventID).Scan(&payload, &exactRecordedAt)
		if err != nil {
			t.Fatal(err)
		}
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatalf("ledger payload disagrees with the relay contract: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"version": "1.1", "id": "paper:" + req.AccountID + ":" + resolution.EventID, "mode": "paper", "source": "paper_ledger",
			"account_id": req.AccountID, "order_id": req.OrderID, "fill_event_id": resolution.EventID,
			"decision_id": req.DecisionID, "quote_id": req.QuoteID, "instrument_id": req.Instrument.ID, "registry_version": req.Instrument.RegistryVersion,
			"side": req.Side, "quantity_raw": "3", "gross_usd": "1", "fee_usd": resolution.FeeUSD, "currency": "USD",
			"research_job_id": req.ResearchJobID,
		} {
			var value string
			if err := json.Unmarshal(got[key], &value); err != nil || value != want {
				t.Fatalf("payload field %s = %s, want %q", key, got[key], want)
			}
		}
		if string(got["decimals"]) != "18" || !exactRecordedAt {
			t.Fatalf("unexpected fill payload: %s", payload)
		}
	}
}

func paperFillWireSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	const name = "https://tradingbot.local/paper-fill-event.schema.json"
	published, err := os.ReadFile("../../../../contracts/paper-fill-event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(published))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(name, value); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestPaperFillOutboxFailureRollsBackAccountingAndFill(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "20")
	req := reserveFixture(1, 1)
	req.GrossUSD, req.MaxFeeUSD = "2", "0.01"
	order, _, err := store.Reserve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	unknown := ResolutionRequest{AccountID: req.AccountID, OrderID: req.OrderID, EventID: identifier(51), Outcome: "unknown"}
	if _, created, err := store.Resolve(ctx, unknown); err != nil || !created {
		t.Fatalf("unknown: created=%v err=%v", created, err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE paper_fill_outbox ADD CONSTRAINT test_reject_enqueue CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	fill := ResolutionRequest{AccountID: req.AccountID, OrderID: req.OrderID, EventID: identifier(52),
		Outcome: "filled", GrossUSD: req.GrossUSD, FeeUSD: req.MaxFeeUSD, ObservedAt: order.CreatedAt}
	if _, created, err := store.Resolve(ctx, fill); err == nil || created {
		t.Fatalf("failed enqueue acknowledged: created=%v err=%v", created, err)
	}
	current, err := store.GetAccount(ctx, req.AccountID)
	if err != nil || current.Version != 3 || current.CashUSD != "20" || len(current.Positions) != 0 {
		t.Fatalf("accounting leaked across rollback: %+v, %v", current, err)
	}
	stored, err := store.GetOrder(ctx, req.AccountID, req.OrderID)
	if err != nil || stored.Status != "unknown" || len(stored.Fills) != 0 {
		t.Fatalf("fill leaked across rollback: %+v, %v", stored, err)
	}
	var eventCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM paper_events WHERE event_id=$1`, fill.EventID).Scan(&eventCount); err != nil || eventCount != 0 {
		t.Fatalf("event leaked across rollback: count=%d err=%v", eventCount, err)
	}
	var outboxCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM paper_fill_outbox`).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("non-fill or failed transaction published: count=%d err=%v", outboxCount, err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE paper_fill_outbox DROP CONSTRAINT test_reject_enqueue`); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.Resolve(ctx, fill); err != nil || !created {
		t.Fatalf("retry: created=%v err=%v", created, err)
	}
	fill.FeeUSD = "0"
	if _, _, err := store.Resolve(ctx, fill); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting retry: %v", err)
	}
}
