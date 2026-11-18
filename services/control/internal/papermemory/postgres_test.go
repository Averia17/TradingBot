package papermemory

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"tradingbot/control/internal/ledger"
)

func TestOutcomeProjectionMatchesPostgresLedger(t *testing.T) {
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("test database must be a PostgreSQL URI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "paper_outcomes_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer base.Close(cleanup)
		if _, err := base.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	params := parsed.Query()
	params.Set("search_path", schema)
	parsed.RawQuery = params.Encode()
	store, err := ledger.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, created, err := store.CreateAccount(ctx, ledger.CreateAccount{ID: testAccount, InitialCashUSD: "1000"}); err != nil || !created {
		t.Fatalf("create paper account: %v", err)
	}

	settle := func(orderID int, version int64, side, units, gross, fee string) {
		t.Helper()
		order := ledger.ReservationRequest{
			AccountID: testAccount, OrderID: eventID(orderID), DecisionID: eventID(orderID + 100), QuoteID: eventID(orderID + 200),
			SnapshotVersion: version, Instrument: testInstrument, Side: side, QuantityRaw: units,
			GrossUSD: gross, MaxFeeUSD: fee, ExpiresAt: time.Now().UTC().Add(time.Minute),
		}
		accepted, created, err := store.Reserve(ctx, order)
		if err != nil || !created {
			t.Fatalf("reserve paper order: %v", err)
		}
		resolution := ledger.ResolutionRequest{
			AccountID: testAccount, OrderID: accepted.OrderID, EventID: eventID(orderID + 1), Outcome: "filled",
			GrossUSD: gross, FeeUSD: fee, ObservedAt: accepted.CreatedAt,
		}
		if _, created, err := store.Resolve(ctx, resolution); err != nil || !created {
			t.Fatalf("settle paper fill: %v", err)
		}
	}
	settle(2, 1, "BUY", "3", "10.00000001", "0.00000002")
	settle(4, 3, "SELL", "2", "12", "0.00000001")
	events, err := store.Events(ctx, testAccount, 0, 100)
	if err != nil || len(events) != 5 {
		t.Fatalf("paper event history: len %d, err %v", len(events), err)
	}
	beforeSale, err := Load(ctx, store, testAccount, events[2].RecordedAt)
	if err != nil || len(beforeSale.Realized) != 0 || len(beforeSale.OpenLots) != 1 || beforeSale.OpenLots[0].QuantityRaw != "3" {
		t.Fatalf("before sale = %+v, %v", beforeSale, err)
	}
	view, err := Load(ctx, store, testAccount, events[4].RecordedAt)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.GetAccount(ctx, testAccount)
	if err != nil || account.Version != view.AccountVersion || account.CashUSD != view.CashUSD || len(account.Positions) != 1 || len(view.OpenLots) != 1 || account.Positions[0].QuantityRaw != view.OpenLots[0].QuantityRaw {
		t.Fatalf("paper ledger and outcome view diverged: account %+v, view %+v, %v", account, view, err)
	}
	if view.RealizedNetPnLUSD != "5.33333331" || view.RecordedFeesUSD != "0.00000003" || view.OpenLots[0].CostBasisUSD != "3.33333335" {
		t.Fatalf("unexpected amounts: %+v", view)
	}

	order, created, err := store.Reserve(ctx, ledger.ReservationRequest{
		AccountID: testAccount, OrderID: eventID(6), DecisionID: eventID(106), QuoteID: eventID(206),
		SnapshotVersion: account.Version, Instrument: testInstrument, Side: "BUY", QuantityRaw: "4",
		GrossUSD: "16", MaxFeeUSD: "0.02", ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil || !created {
		t.Fatalf("reserve partial paper order: %v", err)
	}
	for _, fill := range []ledger.ResolutionRequest{
		{AccountID: testAccount, OrderID: order.OrderID, EventID: eventID(7), Outcome: "partially_filled", QuantityRaw: "2", GrossUSD: "7", FeeUSD: "0.01", ObservedAt: order.CreatedAt},
		{AccountID: testAccount, OrderID: order.OrderID, EventID: eventID(8), Outcome: "filled", GrossUSD: "8", FeeUSD: "0.01", ObservedAt: order.CreatedAt},
	} {
		if _, created, err := store.Resolve(ctx, fill); err != nil || !created {
			t.Fatalf("settle paper quantity: %v", err)
		}
	}
	events, err = store.Events(ctx, testAccount, 5, 100)
	if err != nil || len(events) != 3 {
		t.Fatalf("partial paper event history: len %d, err %v", len(events), err)
	}
	partial, err := Load(ctx, store, testAccount, events[1].RecordedAt)
	if err != nil || len(partial.OpenLots) != 2 || partial.OpenLots[1].QuantityRaw != "2" || partial.OpenLots[1].CostBasisUSD != "7.01" {
		t.Fatalf("unexpected partial snapshot: %+v, %v", partial, err)
	}
	final, err := Load(ctx, store, testAccount, events[2].RecordedAt)
	if err != nil || len(final.OpenLots) != 3 || final.OpenLots[2].QuantityRaw != "2" || final.OpenLots[2].CostBasisUSD != "8.01" {
		t.Fatalf("unexpected final snapshot: %+v, %v", final, err)
	}
	account, err = store.GetAccount(ctx, testAccount)
	if err != nil || account.Version != final.AccountVersion || account.CashUSD != final.CashUSD || account.ReservedCashUSD != "0" ||
		len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "5" || final.RecordedFeesUSD != "0.02000003" {
		t.Fatalf("final projection diverged from ledger: account %+v, view %+v, %v", account, final, err)
	}
}
