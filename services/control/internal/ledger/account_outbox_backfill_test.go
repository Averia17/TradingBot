package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestAccountOutboxBackfillMatchesLiveVersionsAndWireSchema(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "20")
	buy := reserveFixture(1, 1)
	buy.ResearchJobID = identifier(90)
	buy.QuantityRaw, buy.GrossUSD, buy.MaxFeeUSD = "3", "10.00000001", "0.00000003"
	order, _, err := store.Reserve(ctx, buy)
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []ResolutionRequest{
		{OrderID: buy.OrderID, EventID: identifier(51), Outcome: "partially_filled", QuantityRaw: "1", GrossUSD: "3.33333330", FeeUSD: "0.00000001", ObservedAt: order.CreatedAt},
		{OrderID: buy.OrderID, EventID: identifier(52), Outcome: "unknown"},
		{OrderID: buy.OrderID, EventID: identifier(53), Outcome: "cancelled"},
	} {
		outcome.AccountID = testAccountID
		if _, created, err := store.Resolve(ctx, outcome); err != nil || !created {
			t.Fatalf("buy %s: created=%v err=%v", outcome.Outcome, created, err)
		}
	}
	sell := reserveFixture(2, 5)
	sell.Side, sell.QuantityRaw, sell.GrossUSD, sell.MaxFeeUSD = "SELL", "1", "4", "0.01"
	order, _, err = store.Reserve(ctx, sell)
	if err != nil {
		t.Fatal(err)
	}
	fill := ResolutionRequest{AccountID: testAccountID, OrderID: sell.OrderID, EventID: identifier(54), Outcome: "filled",
		GrossUSD: "4", FeeUSD: "0.01", ObservedAt: order.CreatedAt}
	if _, created, err := store.Resolve(ctx, fill); err != nil || !created {
		t.Fatalf("sell: created=%v err=%v", created, err)
	}
	if _, created, err := store.Resolve(ctx, fill); err != nil || created {
		t.Fatalf("sell replay: created=%v err=%v", created, err)
	}

	live := accountOutboxPayloads(t, ctx, store)
	if len(live) != 7 {
		t.Fatalf("got %d account events, want 7", len(live))
	}
	schema := accountOutboxWireSchema(t)
	for i, raw := range live {
		value, err := jsonschema.UnmarshalJSON(bytes.NewBufferString(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatalf("version %d violates wire schema: %v", i+1, err)
		}
	}
	var partial, uncertain, cancelled, sellReservation, closed accountAnalyticsEvent
	for index, destination := range map[int]*accountAnalyticsEvent{2: &partial, 3: &uncertain, 4: &cancelled, 5: &sellReservation, 6: &closed} {
		if err := json.Unmarshal([]byte(live[index]), destination); err != nil {
			t.Fatal(err)
		}
	}
	if partial.CashUSD != "16.66666669" || partial.ReservedCashUSD != "6.6666667" || partial.Fill == nil || partial.Fill.ResearchJobID != buy.ResearchJobID {
		t.Fatalf("bad partial state: %+v", partial)
	}
	if uncertain.CashUSD != partial.CashUSD || uncertain.ReservedCashUSD != partial.ReservedCashUSD || uncertain.Fill != nil ||
		cancelled.CashUSD != partial.CashUSD || cancelled.ReservedCashUSD != "0" || cancelled.Fill != nil {
		t.Fatalf("bad unknown or cancelled state: %+v %+v", uncertain, cancelled)
	}
	if sellReservation.Position == nil || sellReservation.Position.ReservedQuantityRaw != "1" || closed.Position == nil ||
		closed.Position.QuantityRaw != "0" || closed.Position.ReservedQuantityRaw != "0" || closed.Fill == nil ||
		closed.Fill.QuantityRaw != "1" || closed.CashUSD != "20.65666669" {
		t.Fatalf("bad sale: %+v %+v", sellReservation, closed)
	}
	// Re-run the migration in this isolated test schema against the same
	// legacy ledger rows, with the populated outbox and its view removed.
	if _, err := store.pool.Exec(ctx, `DROP VIEW paper_account_analytics_heads; DROP TABLE paper_account_outbox;
		DELETE FROM paper_schema_migrations WHERE version=$1`, pgx.QueryExecModeSimpleProtocol, accountOutboxMigrationVersion); err != nil {
		t.Fatal(err)
	}
	store.Close()
	reopened := openLedger(t, ctx, dsn)
	if rebuilt := accountOutboxPayloads(t, ctx, reopened); !reflect.DeepEqual(rebuilt, live) {
		t.Fatalf("rebuilt events differ from transactional exports\nrebuilt=%v\nlive=%v", rebuilt, live)
	}
}

func TestAccountOutboxFailureRollsBackLedgerAndFill(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "20")
	buy := reserveFixture(1, 1)
	buy.GrossUSD, buy.MaxFeeUSD = "10", "0"
	order, _, err := store.Reserve(ctx, buy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE paper_account_outbox ADD CONSTRAINT test_reject_enqueues CHECK (account_version < 3)`); err != nil {
		t.Fatal(err)
	}
	fill := ResolutionRequest{AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(51), Outcome: "filled",
		GrossUSD: "10", FeeUSD: "0", ObservedAt: order.CreatedAt}
	if _, created, err := store.Resolve(ctx, fill); err == nil || created {
		t.Fatalf("account outbox failure acknowledged: created=%v err=%v", created, err)
	}
	account, err := store.GetAccount(ctx, testAccountID)
	if err != nil || account.Version != 2 || account.CashUSD != "20" || account.ReservedCashUSD != "10" || len(account.Positions) != 0 {
		t.Fatalf("failed outbox modified the account: %+v err=%v", account, err)
	}
	var fills, notifications int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM paper_fills), (SELECT count(*) FROM paper_fill_outbox)`).Scan(&fills, &notifications); err != nil || fills != 0 || notifications != 0 {
		t.Fatalf("failed account outbox leaked a fill or notification: %d, %d, %v", fills, notifications, err)
	}
}

func accountOutboxPayloads(t *testing.T, ctx context.Context, store *Store) []string {
	t.Helper()
	rows, err := store.pool.Query(ctx, `SELECT payload::text FROM paper_account_outbox WHERE account_id=$1 ORDER BY account_version`, testAccountID)
	if err != nil {
		t.Fatal(err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return payloads
}

func accountOutboxWireSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	const name = "https://tradingbot.local/paper-account-event.schema.json"
	data, err := os.ReadFile("../../../../contracts/paper-account-event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
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
