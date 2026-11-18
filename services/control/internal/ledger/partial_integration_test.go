package ledger

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPartialBuyRetainsExactResidualAcrossRestartAndReplay(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "20")
	buy := reserveFixture(1, 1)
	buy.QuantityRaw, buy.GrossUSD, buy.MaxFeeUSD = "3", "10.00000001", "0.00000003"
	order, _, err := store.Reserve(ctx, buy)
	if err != nil {
		t.Fatal(err)
	}
	fill := ResolutionRequest{AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(51), Outcome: "partially_filled", QuantityRaw: "1", GrossUSD: "3.33333334", FeeUSD: "0.00000001", ObservedAt: order.CreatedAt}
	if _, _, err := store.Resolve(ctx, fill); !errors.Is(err, ErrInvalid) {
		t.Fatalf("buy above its proportional ceiling: %v", err)
	}
	fill.GrossUSD = "3.33333330"
	first, created, err := store.Resolve(ctx, fill)
	if err != nil || !created || first.Status != "partially_filled" || first.RemainingQuantityRaw != "2" || first.FilledQuantityRaw != "1" || len(first.Fills) != 1 || first.Fill == nil || first.Fill.QuantityRaw != "1" {
		t.Fatalf("partial result = %+v, %v, %v", first, created, err)
	}
	if _, _, err := store.Resolve(ctx, ResolutionRequest{AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(52), Outcome: "unknown"}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store = openLedger(t, ctx, dsn)
	account, err := store.GetAccount(ctx, testAccountID)
	if err != nil || account.CashUSD != "16.66666669" || account.ReservedCashUSD != "6.6666667" || len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "1" {
		t.Fatalf("uncertain residual = %+v, %v", account, err)
	}
	last := ResolutionRequest{AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(53), Outcome: "filled", GrossUSD: "6.66666660", FeeUSD: "0.00000003", ObservedAt: order.CreatedAt}
	if _, _, err := store.Resolve(ctx, last); !errors.Is(err, ErrInvalid) {
		t.Fatalf("aggregate fees above cap: %v", err)
	}
	last.FeeUSD = "0.00000002"
	settled, created, err := store.Resolve(ctx, last)
	if err != nil || !created || settled.Status != "filled" || settled.RemainingQuantityRaw != "0" || settled.FilledQuantityRaw != "3" || len(settled.Fills) != 2 || settled.Fills[1].QuantityRaw != "2" {
		t.Fatalf("full residual result = %+v, %v, %v", settled, created, err)
	}
	if replay, created, err := store.Resolve(ctx, fill); err != nil || created || !reflect.DeepEqual(replay, first) {
		t.Fatalf("partial fill replay changed its receipt: %+v, %v, %v", replay, created, err)
	}
	fill.GrossUSD = "3.33333329"
	if _, _, err := store.Resolve(ctx, fill); !errors.Is(err, ErrConflict) {
		t.Fatalf("accepted a conflicting partial fill replay: %v", err)
	}
	account, err = store.GetAccount(ctx, testAccountID)
	if err != nil || account.CashUSD != "10.00000007" || account.ReservedCashUSD != "0" || len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "3" {
		t.Fatalf("settled balance = %+v, %v", account, err)
	}
}

func TestCancelBuyAfterPartialFillReleasesOnlyItsResidual(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "20")
	buy := reserveFixture(1, 1)
	buy.QuantityRaw, buy.GrossUSD, buy.MaxFeeUSD = "3", "10.00000001", "0.00000003"
	order, _, err := store.Reserve(ctx, buy)
	if err != nil {
		t.Fatal(err)
	}
	fill := ResolutionRequest{AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(51), Outcome: "partially_filled", QuantityRaw: "1", GrossUSD: "3.33333330", FeeUSD: "0.00000001", ObservedAt: order.CreatedAt}
	if _, _, err := store.Resolve(ctx, fill); err != nil {
		t.Fatal(err)
	}
	other := reserveFixture(2, 3)
	other.GrossUSD, other.MaxFeeUSD = "4", "0.01"
	if _, _, err := store.Reserve(ctx, other); err != nil {
		t.Fatal(err)
	}
	cancel := ResolutionRequest{AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(52), Outcome: "cancelled"}
	settled, created, err := store.Resolve(ctx, cancel)
	if err != nil || !created || settled.Status != "cancelled" || settled.RemainingQuantityRaw != "2" || settled.FilledQuantityRaw != "1" {
		t.Fatalf("partial cancellation = %+v, %v, %v", settled, created, err)
	}
	account, err := store.GetAccount(ctx, testAccountID)
	if err != nil || account.CashUSD != "16.66666669" || account.ReservedCashUSD != "4.01" || len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "1" {
		t.Fatalf("cancellation changed settled amounts or another reservation: %+v, %v", account, err)
	}
}

func TestConcurrentPartialSalesNeverDisposeOfResidualTwice(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "24")
	buy := reserveFixture(1, 1)
	buy.QuantityRaw, buy.GrossUSD, buy.MaxFeeUSD = "3", "12", "0"
	order, _, err := store.Reserve(ctx, buy)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resolve(ctx, ResolutionRequest{AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(51), Outcome: "filled", GrossUSD: "12", FeeUSD: "0", ObservedAt: order.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	sell := reserveFixture(2, 3)
	sell.Side, sell.QuantityRaw, sell.GrossUSD, sell.MaxFeeUSD = "SELL", "3", "12.00000001", "0.03"
	order, _, err = store.Reserve(ctx, sell)
	if err != nil {
		t.Fatal(err)
	}
	below := ResolutionRequest{AccountID: testAccountID, OrderID: sell.OrderID, EventID: identifier(52), Outcome: "partially_filled", QuantityRaw: "1", GrossUSD: "4", FeeUSD: "0.01", ObservedAt: order.CreatedAt}
	if _, _, err := store.Resolve(ctx, below); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted a partial fill below price floor: %v", err)
	}
	below.GrossUSD, below.ObservedAt = "4.00000001", sell.ExpiresAt
	if _, _, err := store.Resolve(ctx, below); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted a partial fill outside its window: %v", err)
	}
	var wait sync.WaitGroup
	const requests = 12
	results := make(chan error, requests)
	for index := range requests {
		wait.Go(func() {
			_, _, err := store.Resolve(ctx, ResolutionRequest{AccountID: testAccountID, OrderID: sell.OrderID, EventID: identifier(60 + index), Outcome: "partially_filled", QuantityRaw: "1", GrossUSD: "4.00000001", FeeUSD: "0.01", ObservedAt: order.CreatedAt})
			results <- err
		})
	}
	wait.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if accepted != 2 {
		t.Fatalf("expected two partial executions, got %d", accepted)
	}
	account, err := store.GetAccount(ctx, testAccountID)
	if err != nil || account.CashUSD != "19.98000002" || len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "1" || account.Positions[0].ReservedQuantityRaw != "1" {
		t.Fatalf("unfilled inventory missing or future proceeds credited: %+v, %v", account, err)
	}
	if _, _, err := store.Resolve(ctx, ResolutionRequest{AccountID: testAccountID, OrderID: sell.OrderID, EventID: identifier(90), Outcome: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	account, err = store.GetAccount(ctx, testAccountID)
	if err != nil || len(account.Positions) != 1 || account.Positions[0].ReservedQuantityRaw != "0" || account.Positions[0].QuantityRaw != "1" || account.CashUSD != "19.98000002" {
		t.Fatalf("cancel changed settled inventory or cash: %+v, %v", account, err)
	}
}

func TestMigrateLegacyFullFillAndReplayItsOriginalRequest(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, statement := range []string{accountsMigration, ordersMigration, `CREATE TABLE paper_schema_migrations (
		version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
	); INSERT INTO paper_schema_migrations(version) VALUES (1), (2)`} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	creation, err := json.Marshal(CreateAccount{ID: testAccountID, InitialCashUSD: "20"})
	if err != nil {
		t.Fatal(err)
	}
	creationHash := sha256.Sum256(creation)
	buy := reserveFixture(1, 1)
	buy.QuantityRaw, buy.GrossUSD, buy.MaxFeeUSD = "3", "12", "0.01"
	buy.ExpiresAt = buy.ExpiresAt.Truncate(time.Microsecond)
	observed := time.Now().UTC().Truncate(time.Microsecond)
	fill := ResolutionRequest{AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(51), Outcome: "filled", GrossUSD: "11", FeeUSD: "0.01", ObservedAt: observed}
	booking, err := json.Marshal(buy)
	if err != nil {
		t.Fatal(err)
	}
	// Serialize the pre-migration shape independently of the new request struct.
	legacy := fmt.Appendf(nil, `{"account_id":%q,"order_id":%q,"event_id":%q,"outcome":"filled","gross_usd":"11","fee_usd":"0.01","observed_at":%q}`,
		buy.AccountID, buy.OrderID, fill.EventID, observed.Format(time.RFC3339Nano))
	bookingHash, outcomeHash := sha256.Sum256(booking), sha256.Sum256(legacy)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `INSERT INTO paper_accounts (account_id, creation_hash, cash_usd) VALUES ($1,$2,20)`, buy.AccountID, creationHash[:]); err != nil {
		t.Fatal(err)
	}
	if err := writeEvent(ctx, tx, buy.AccountID, buy.AccountID, 1, "paper_account.created", creationHash[:], creation); err != nil {
		t.Fatal(err)
	}
	if err := writeEvent(ctx, tx, buy.AccountID, buy.OrderID, 2, "paper_order.reserved", bookingHash[:], booking); err != nil {
		t.Fatal(err)
	}
	if err := writeEvent(ctx, tx, buy.AccountID, fill.EventID, 3, "paper_order.filled", outcomeHash[:], legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_orders (account_id, order_id, decision_id, quote_id, request, request_hash, status, account_version, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,'filled',3,$7)`, buy.AccountID, buy.OrderID, buy.DecisionID, buy.QuoteID, booking, bookingHash[:], observed); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_fills (account_id, order_id, event_id, gross_usd, fee_usd, observed_at)
		VALUES ($1,$2,$3,11,0.01,$4)`, buy.AccountID, buy.OrderID, fill.EventID, observed); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE paper_accounts SET version=3, cash_usd=8.99 WHERE account_id=$1`, buy.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paper_positions (account_id, instrument_id, registry_version, decimals, quantity_raw)
		VALUES ($1,$2,$3,$4,3)`, buy.AccountID, buy.Instrument.ID, buy.Instrument.RegistryVersion, buy.Instrument.Decimals); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	store := openLedger(t, ctx, dsn)
	order, err := store.GetOrder(ctx, buy.AccountID, buy.OrderID)
	if err != nil || order.FilledQuantityRaw != "3" || order.RemainingQuantityRaw != "0" || len(order.Fills) != 1 || order.Fill == nil || order.Fill.QuantityRaw != "3" {
		t.Fatalf("backfilled legacy order = %+v, %v", order, err)
	}
	if replay, created, err := store.Resolve(ctx, fill); err != nil || created || !reflect.DeepEqual(replay, order) {
		t.Fatalf("legacy outcome replay = %+v, %v, %v", replay, created, err)
	}
	var outboxCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM paper_fill_outbox`).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("migrated execution was silently queued: count=%d err=%v", outboxCount, err)
	}
	var migratedQuantity string
	if err := store.pool.QueryRow(ctx, `SELECT payload #>> '{fill,quantity_raw}' FROM paper_account_outbox WHERE account_id=$1 AND account_version=3`,
		buy.AccountID).Scan(&migratedQuantity); err != nil || migratedQuantity != "3" {
		t.Fatalf("legacy booked quantity missing from analytics history: quantity=%s err=%v", migratedQuantity, err)
	}
	account, err := store.GetAccount(ctx, buy.AccountID)
	if err != nil || account.Version != 3 || account.CashUSD != "8.99" || len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "3" {
		t.Fatalf("migration or replay changed settled amounts: %+v, %v", account, err)
	}
}
