package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func identifier(n int) string { return fmt.Sprintf("e36eed0a-2a01-44ce-bd46-%012d", n) }

func reserveFixture(order int, version int64) ReservationRequest {
	return ReservationRequest{
		AccountID: testAccountID, OrderID: identifier(order), DecisionID: identifier(order + 100), QuoteID: identifier(order + 200),
		SnapshotVersion: version, Instrument: Instrument{ID: "offline:TSLA", RegistryVersion: "offline-1", Decimals: 8},
		Side: "BUY", QuantityRaw: "100000001", GrossUSD: "20.12345678", MaxFeeUSD: "0.00000002", ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
}

func createFixture(t *testing.T, ctx context.Context, store *Store, usd string) {
	t.Helper()
	if _, created, err := store.CreateAccount(ctx, CreateAccount{ID: testAccountID, InitialCashUSD: usd}); err != nil || !created {
		t.Fatalf("create paper fixture = %v, %v", created, err)
	}
}

func TestPaperUnknownKeepsReservationAndSettleDebitsExactAmounts(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "50.00000001")
	request := reserveFixture(1, 1)
	reserved, created, err := store.Reserve(ctx, request)
	if err != nil || !created || reserved.Status != "reserved" || reserved.AccountVersion != 2 {
		t.Fatalf("reserve = %+v, %v, %v", reserved, created, err)
	}
	unknown := ResolutionRequest{AccountID: testAccountID, OrderID: request.OrderID, EventID: identifier(51), Outcome: "unknown"}
	if order, created, err := store.Resolve(ctx, unknown); err != nil || !created || order.Status != "unknown" {
		t.Fatalf("mark unknown = %+v, %v, %v", order, created, err)
	}
	store.Close()
	store = openLedger(t, ctx, dsn)
	account, err := store.GetAccount(ctx, testAccountID)
	if err != nil || account.CashUSD != "50.00000001" || account.ReservedCashUSD != "20.1234568" || account.Version != 3 {
		t.Fatalf("unconfirmed order lost its reservation: %+v, %v", account, err)
	}
	tooBig := reserveFixture(2, account.Version)
	tooBig.GrossUSD = "30"
	if _, _, err := store.Reserve(ctx, tooBig); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("unavailable cash = %v", err)
	}
	if order, created, err := store.Reserve(ctx, request); err != nil || created || order.Status != "unknown" {
		t.Fatalf("replay reservation = %+v, %v, %v", order, created, err)
	}
	conflict := request
	conflict.GrossUSD = "1"
	if _, _, err := store.Reserve(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed order identity = %v", err)
	}
	fill := ResolutionRequest{AccountID: testAccountID, OrderID: request.OrderID, EventID: identifier(52), Outcome: "filled", GrossUSD: "20.12345677", FeeUSD: "0.00000003", ObservedAt: reserved.CreatedAt}
	if _, _, err := store.Resolve(ctx, fill); !errors.Is(err, ErrInvalid) {
		t.Fatalf("fee above reservation = %v", err)
	}
	fill.FeeUSD = "0.00000001"
	filled, created, err := store.Resolve(ctx, fill)
	if err != nil || !created || filled.Status != "filled" || filled.Fill == nil || filled.Fill.GrossUSD != fill.GrossUSD || filled.Fill.FeeUSD != fill.FeeUSD || filled.AccountVersion != 4 {
		t.Fatalf("settle = %+v, %v, %v", filled, created, err)
	}
	if order, created, err := store.Resolve(ctx, fill); err != nil || created || order.Fill == nil || order.Fill.EventID != fill.EventID {
		t.Fatalf("replay resolution = %+v, %v, %v", order, created, err)
	}
	account, err = store.GetAccount(ctx, testAccountID)
	if err != nil || account.CashUSD != "29.87654323" || account.ReservedCashUSD != "0" || account.Version != 4 || len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "100000001" {
		t.Fatalf("final account = %+v, %v", account, err)
	}
	events, err := store.Events(ctx, testAccountID, 1, 10)
	if err != nil || len(events) != 3 || events[0].Kind != "paper_order.reserved" || events[1].Kind != "paper_order.unknown" || events[2].Kind != "paper_order.filled" {
		t.Fatalf("audit events = %+v, %v", events, err)
	}
}

func TestPaperSellCannotShortOrSpendFutureProceeds(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "25")
	buy := reserveFixture(1, 1)
	opening, _, err := store.Reserve(ctx, buy)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resolve(ctx, ResolutionRequest{AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(51), Outcome: "filled", GrossUSD: "20", FeeUSD: "0.00000001", ObservedAt: opening.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	sell := reserveFixture(2, 3)
	sell.Side = "SELL"
	sell.QuantityRaw = "100000002"
	if _, _, err := store.Reserve(ctx, sell); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("sell above holdings = %v", err)
	}
	sell.QuantityRaw = buy.QuantityRaw
	if _, _, err := store.Reserve(ctx, sell); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Reserve(ctx, reserveFixture(3, 4)); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("spent unreceived sale proceeds = %v", err)
	}
	if _, _, err := store.Resolve(ctx, ResolutionRequest{AccountID: testAccountID, OrderID: sell.OrderID, EventID: identifier(52), Outcome: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resolve(ctx, ResolutionRequest{AccountID: testAccountID, OrderID: sell.OrderID, EventID: identifier(53), Outcome: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	account, err := store.GetAccount(ctx, testAccountID)
	if err != nil || account.CashUSD != "4.99999999" || account.Version != 6 || len(account.Positions) != 1 || account.Positions[0].QuantityRaw != buy.QuantityRaw || account.Positions[0].ReservedQuantityRaw != "0" {
		t.Fatalf("cancellation changed owned cash or inventory: %+v, %v", account, err)
	}
	sell = reserveFixture(4, account.Version)
	sell.Side = "SELL"
	resale, _, err := store.Reserve(ctx, sell)
	if err != nil {
		t.Fatal(err)
	}
	settlement := ResolutionRequest{AccountID: testAccountID, OrderID: sell.OrderID, EventID: identifier(54), Outcome: "filled", GrossUSD: "21", FeeUSD: "0.00000002", ObservedAt: resale.CreatedAt}
	if _, _, err := store.Resolve(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	account, err = store.GetAccount(ctx, testAccountID)
	if err != nil || account.CashUSD != "25.99999997" || account.ReservedCashUSD != "0" || len(account.Positions) != 0 {
		t.Fatalf("sell settlement failed to realize exact proceeds and remove holdings: %+v, %v", account, err)
	}
}

func TestPaperReservationsRejectExpiredAndConcurrentStaleVersions(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "30")
	expired := reserveFixture(1, 1)
	expired.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if _, _, err := store.Reserve(ctx, expired); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired quote = %v", err)
	}
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for orderID := 2; orderID <= 3; orderID++ {
		wait.Go(func() {
			_, _, err := store.Reserve(ctx, reserveFixture(orderID, 1))
			results <- err
		})
	}
	wait.Wait()
	close(results)
	var accepted, stale int
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrStale):
			stale++
		default:
			t.Fatalf("concurrent reservation = %v", err)
		}
	}
	if accepted != 1 || stale != 1 {
		t.Fatalf("concurrent reservations: accepted=%d stale=%d", accepted, stale)
	}
	incompatible := reserveFixture(4, 2)
	incompatible.Instrument.Decimals = 7
	if _, _, err := store.Reserve(ctx, incompatible); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending reservation allowed incompatible instrument units: %v", err)
	}
}
