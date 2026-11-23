package accounts

import (
	"errors"
	"testing"
	"time"
)

func TestRiskActivityCountsSettledGrossAndUnresolvedResidualAtAccountVersion(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "500")

	buy := reserveFixture(1, 1)
	buy.QuantityRaw, buy.GrossUSD = "300000000", "60"
	reserved, _, err := store.Reserve(ctx, buy)
	if err != nil {
		t.Fatal(err)
	}
	partial := ResolutionRequest{
		AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(51),
		Outcome: "partially_filled", QuantityRaw: "100000000", GrossUSD: "17.00000001",
		FeeUSD: "0.00000001", ObservedAt: reserved.CreatedAt,
	}
	bought, _, err := store.Resolve(ctx, partial)
	if err != nil {
		t.Fatal(err)
	}
	sell := reserveFixture(2, bought.AccountVersion)
	sell.Side, sell.QuantityRaw, sell.GrossUSD = "SELL", "100000000", "19"
	reservedSell, _, err := store.Reserve(ctx, sell)
	if err != nil {
		t.Fatal(err)
	}
	sold, _, err := store.Resolve(ctx, ResolutionRequest{
		AccountID: testAccountID, OrderID: sell.OrderID, EventID: identifier(52), Outcome: "filled",
		GrossUSD: "24.00000002", FeeUSD: "0.00000001", ObservedAt: reservedSell.CreatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity, err := store.RiskActivity(ctx, testAccountID, sold.AccountVersion)
	wantPending := RiskReservation{
		Instrument: buy.Instrument, QuantityRaw: "300000000", RemainingQuantityRaw: "200000000", GrossUSD: "60",
	}
	if err != nil || activity.AccountVersion != sold.AccountVersion || activity.DayUTC.Location() != time.UTC ||
		activity.DayUTC.Hour() != 0 || activity.DayUTC.Minute() != 0 || activity.SettledGrossUSD != "41.00000003" ||
		activity.HasUnknown || len(activity.Pending) != 1 || activity.Pending[0] != wantPending {
		t.Fatalf("partially settled daily activity = %+v, %v", activity, err)
	}
	unknown, _, err := store.Resolve(ctx, ResolutionRequest{
		AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(53), Outcome: "unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.RiskActivity(ctx, testAccountID, sold.AccountVersion); !errors.Is(err, ErrStale) {
		t.Fatalf("stale activity version = %v", err)
	}
	if _, _, err = store.Reserve(ctx, reserveFixture(3, activity.AccountVersion)); !errors.Is(err, ErrStale) {
		t.Fatalf("a concurrent authoritative change must invalidate the admission version: %v", err)
	}
	activity, err = store.RiskActivity(ctx, testAccountID, unknown.AccountVersion)
	if err != nil || !activity.HasUnknown || activity.SettledGrossUSD != "41.00000003" ||
		len(activity.Pending) != 1 || activity.Pending[0] != wantPending {
		t.Fatalf("unresolved daily activity = %+v, %v", activity, err)
	}
	pending := reserveFixture(3, unknown.AccountVersion)
	reservedAgain, _, err := store.Reserve(ctx, pending)
	if err != nil {
		t.Fatal(err)
	}
	wantNew := RiskReservation{Instrument: pending.Instrument, QuantityRaw: pending.QuantityRaw,
		RemainingQuantityRaw: pending.QuantityRaw, GrossUSD: pending.GrossUSD}
	activity, err = store.RiskActivity(ctx, testAccountID, reservedAgain.AccountVersion)
	if err != nil || !activity.HasUnknown || len(activity.Pending) != 2 ||
		activity.Pending[0] != wantPending || activity.Pending[1] != wantNew {
		t.Fatalf("activity with multiple open orders = %+v, %v", activity, err)
	}

	// In this isolated fixture, put the sell's record immediately before UTC midnight.
	if _, err := store.pool.Exec(ctx, `UPDATE paper_events SET recorded_at=$3
		WHERE account_id=$1 AND event_id=$2`, testAccountID, identifier(52), activity.DayUTC.Add(-time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	cancelled, _, err := store.Resolve(ctx, ResolutionRequest{
		AccountID: testAccountID, OrderID: buy.OrderID, EventID: identifier(54), Outcome: "cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	activity, err = store.RiskActivity(ctx, testAccountID, cancelled.AccountVersion)
	if err != nil || activity.HasUnknown || activity.SettledGrossUSD != "17.00000001" ||
		len(activity.Pending) != 1 || activity.Pending[0] != wantNew {
		t.Fatalf("cancelled residual and UTC-day activity = %+v, %v", activity, err)
	}
}
