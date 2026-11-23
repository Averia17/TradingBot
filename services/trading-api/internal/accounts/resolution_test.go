package accounts

import (
	"errors"
	"math/big"
	"slices"
	"testing"
	"time"
)

func TestCompareAmountsMatchesExactDecimalOrdering(t *testing.T) {
	values := []string{
		"0", "0.00000000", "0.00000001", "0.09999999", "0.1", "0.10000000", "0.10000001",
		"1", "1.00000001", "9.99999999", "10", "999999999999999999999999999999.12345678",
		"999999999999999999999999999999.12345679",
	}
	for _, left := range values {
		for _, right := range values {
			if got, want := compareAmounts(left, right), amount(left).Cmp(amount(right)); got != want {
				t.Fatalf("compareAmounts(%q, %q) = %d, want %d", left, right, got, want)
			}
		}
	}
}

func TestAppendSettledFillMatchesReplayedSummary(t *testing.T) {
	order := Order{
		ReservationRequest: ReservationRequest{QuantityRaw: "99999999999999999999999999999999999999"},
		Status:             "reserved",
	}
	steps := []struct {
		status, quantity, remaining string
	}{
		{"partially_filled", "1", "99999999999999999999999999999999999998"},
		{"filled", "99999999999999999999999999999999999998", "0"},
	}
	for i, step := range steps {
		fill := Fill{EventID: identifier(50 + i), QuantityRaw: step.quantity,
			GrossUSD: "0.00000001", FeeUSD: "0", ObservedAt: time.Unix(int64(i+1), 0).UTC()}
		order.Status = step.status
		order.appendSettledFill(fill, rawUnits(step.remaining))
		replayed := Order{ReservationRequest: order.ReservationRequest, Status: order.Status, Fills: slices.Clone(order.Fills)}
		if err := replayed.validateAndSummarizeFills(); err != nil {
			t.Fatal(err)
		}
		if order.FilledQuantityRaw != replayed.FilledQuantityRaw || order.RemainingQuantityRaw != replayed.RemainingQuantityRaw ||
			len(order.Fills) != i+1 || order.Fill != &order.Fills[i] || *order.Fill != *replayed.Fill {
			t.Fatalf("incremental summary %+v differs from stored history %+v", order, replayed)
		}
	}
}

func TestBuyReservationReleaseRoundsEachRemainingBalance(t *testing.T) {
	order := ReservationRequest{QuantityRaw: "3", GrossUSD: "10.00000001", MaxFeeUSD: "0.00000005"}
	initial := orderReserve{quantity: rawUnits(order.QuantityRaw), fee: amount(order.MaxFeeUSD)}
	first := initial.afterFill(rawUnits("1"), amount("0.00000001"))
	second := first.afterFill(rawUnits("1"), amount("0.00000002"))
	closed := orderReserve{quantity: new(big.Int), fee: new(big.Rat)}

	if got := initial.buyCash(order).FloatString(8); got != "10.00000006" {
		t.Fatalf("initial cash reserve = %s", got)
	}
	for _, step := range []struct {
		before, after orderReserve
		want          string
	}{
		{initial, first, "3.33333334"},
		{first, second, "3.33333336"},
		{second, closed, "3.33333336"},
	} {
		if got := step.before.releasedBuyCash(order, step.after).FloatString(8); got != step.want {
			t.Fatalf("cash released from %s to %s units = %s, want %s", step.before.quantity, step.after.quantity, got, step.want)
		}
	}
}

func TestOrderAccountingProjectsMixedOrderLifecycles(t *testing.T) {
	history := accountHistory{
		orders: make(map[string]orderAccounting), positions: make(map[string]Position),
	}
	history.cash.Set(amount("50"))
	instrument := Instrument{ID: "test", RegistryVersion: "1", Decimals: 8}
	buy := ReservationRequest{OrderID: "buy", Instrument: instrument, Side: "BUY", QuantityRaw: "3", GrossUSD: "10.00000001", MaxFeeUSD: "0.00000005"}
	sell := ReservationRequest{OrderID: "sell", Instrument: instrument, Side: "SELL", QuantityRaw: "2", GrossUSD: "8", MaxFeeUSD: "1"}

	assertBalances := func(cash, reservedCash, quantity, reservedQuantity string) {
		t.Helper()
		pos := history.positions[instrument.ID]
		if analyticsCash(&history.cash) != cash || analyticsCash(&history.reservedCash) != reservedCash ||
			pos.QuantityRaw != quantity || pos.ReservedQuantityRaw != reservedQuantity {
			t.Fatalf("got cash %s reserved %s quantity %s reserved %s, want %s %s %s %s",
				analyticsCash(&history.cash), analyticsCash(&history.reservedCash), pos.QuantityRaw, pos.ReservedQuantityRaw,
				cash, reservedCash, quantity, reservedQuantity)
		}
	}
	resolve := func(live *Order, outcome string, fill *Fill) {
		t.Helper()
		replayed, exists := history.orders[live.OrderID]
		restored := restoreOrderAccounting(*live)
		if !exists || replayed.request != restored.request || replayed.status != restored.status ||
			replayed.remaining.quantity.Cmp(restored.remaining.quantity) != 0 || replayed.remaining.fee.Cmp(restored.remaining.fee) != 0 {
			t.Fatalf("stored order and replay do not agree before %s", outcome)
		}
		if _, err := history.resolve(replayed, outcome, fill); err != nil {
			t.Fatal(err)
		}
		live.Status = outcome
		if fill != nil {
			live.Fills = append(live.Fills, *fill)
		}
		if err := live.validateAndSummarizeFills(); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := history.reserve(buy); err != nil {
		t.Fatal(err)
	}
	assertBalances("50", "10.00000006", "0", "0")
	buyOrder := Order{ReservationRequest: buy, Status: "reserved", RemainingQuantityRaw: "3"}
	resolve(&buyOrder, "unknown", nil)
	if _, err := history.resolve(history.orders[buy.OrderID], "unknown", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeated unknown resolution returned %v", err)
	}
	assertBalances("50", "10.00000006", "0", "0")

	observedAt := time.Unix(1, 0).UTC()
	resolve(&buyOrder, "partially_filled", &Fill{QuantityRaw: "1", GrossUSD: "3.33333330", FeeUSD: "0.00000001", ObservedAt: observedAt})
	assertBalances("46.66666669", "6.66666672", "1", "0")
	resolve(&buyOrder, "partially_filled", &Fill{QuantityRaw: "1", GrossUSD: "3.33333333", FeeUSD: "0.00000002", ObservedAt: observedAt})
	assertBalances("43.33333334", "3.33333336", "2", "0")
	resolve(&buyOrder, "unknown", nil)
	resolve(&buyOrder, "cancelled", nil)
	assertBalances("43.33333334", "0", "2", "0")

	if _, err := history.reserve(sell); err != nil {
		t.Fatal(err)
	}
	assertBalances("43.33333334", "0", "2", "2")
	sellOrder := Order{ReservationRequest: sell, Status: "reserved", RemainingQuantityRaw: "2"}
	resolve(&sellOrder, "partially_filled", &Fill{QuantityRaw: "1", GrossUSD: "4.10", FeeUSD: "0.10", ObservedAt: observedAt})
	assertBalances("47.33333334", "0", "1", "1")
	overFee := Fill{QuantityRaw: "1", GrossUSD: "4.05", FeeUSD: "0.91", ObservedAt: observedAt}
	if _, err := history.resolve(history.orders[sell.OrderID], "filled", &overFee); !errors.Is(err, ErrInvalid) {
		t.Fatalf("fill above remaining fee budget returned %v", err)
	}
	assertBalances("47.33333334", "0", "1", "1")
	resolve(&sellOrder, "filled", &Fill{QuantityRaw: "1", GrossUSD: "4.05", FeeUSD: "0.09", ObservedAt: observedAt})
	assertBalances("51.29333334", "0", "0", "0")
	if len(history.orders) != 0 {
		t.Fatalf("closed orders remain in history: %+v", history.orders)
	}
}
