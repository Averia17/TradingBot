package papernav

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/control/internal/ledger"
)

const account = "00000000-0000-0000-0000-000000000001"

var asset = ledger.Instrument{ID: "stock:ABC", RegistryVersion: "fixture-v1", Decimals: 2}

func eventID(number int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", number) }

func stamp(text string) time.Time {
	result, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		panic(err)
	}
	return result
}

func ledgerEvent(t *testing.T, number int, kind, received string, payload any) ledger.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return ledger.Event{
		ID: eventID(number), AccountVersion: int64(number), Kind: kind,
		RecordedAt: stamp(received), Payload: raw,
	}
}

type history struct {
	events    []ledger.Event
	requested []int64
}

func (h *history) Events(_ context.Context, id string, after int64, _ int) ([]ledger.Event, error) {
	if id != account {
		return nil, fmt.Errorf("unexpected account %s", id)
	}
	h.requested = append(h.requested, after)
	if after >= int64(len(h.events)) {
		return nil, nil
	}
	return h.events[after:min(after+2, int64(len(h.events)))], nil
}

func newRequest() Request {
	return Request{
		Version: "0.1", AccountID: account, StartDay: "2026-01-01", EndDay: "2026-01-04",
		MarkMaxAgeSeconds: 3600, Source: Source{Kind: "synthetic_fixture", ID: "fixture-A"},
		Marks: []Mark{},
	}
}

func pinned(t *testing.T, req Request) Input {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	input, err := Parse(raw, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func create(t *testing.T) ledger.Event {
	t.Helper()
	return ledgerEvent(t, 1, "paper_account.created", "2026-01-01T12:00:00Z", ledger.CreateAccount{ID: account, InitialCashUSD: "1000"})
}

func order(t *testing.T, version int, instrument ledger.Instrument, qty, received string) ledger.Event {
	t.Helper()
	return ledgerEvent(t, version, "paper_order.reserved", received, ledger.ReservationRequest{
		AccountID: account, OrderID: eventID(version), DecisionID: eventID(900 + version),
		SnapshotVersion: int64(version - 1), Instrument: instrument, Side: "BUY", QuantityRaw: qty,
	})
}

func fill(t *testing.T, version, orderVersion int, kind, qty, gross, fee, at, recorded string) ledger.Event {
	t.Helper()
	return ledgerEvent(t, version, "paper_order."+kind, recorded, ledger.ResolutionRequest{
		AccountID: account, OrderID: eventID(orderVersion), EventID: eventID(version), Outcome: kind,
		QuantityRaw: qty, GrossUSD: gross, FeeUSD: fee, ObservedAt: stamp(at),
	})
}

func mark(instrument ledger.Instrument, id, price, observed, available string) Mark {
	return Mark{
		ID: id, Instrument: instrument, PriceUSD: price, ObservedAt: stamp(observed), AvailableAt: stamp(available),
	}
}

func stringValue(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

func validateReportSchema(t *testing.T, raw []byte) {
	t.Helper()
	data, err := os.ReadFile("../../../../contracts/paper-nav.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	const root = "https://tradingbot.local/paper-nav.schema.json"
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(root, value); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile(root + "#/$defs/Report")
	if err != nil {
		t.Fatal(err)
	}
	report, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(report); err != nil {
		t.Fatal(err)
	}
}

func TestReportUsesRecordedEventsAndAvailableMarksAtUTCClosing(t *testing.T) {
	req := newRequest()
	req.Marks = []Mark{
		mark(asset, "late_revision", "100", "2026-01-02T23:00:00Z", "2026-01-05T00:00:00Z"),
		mark(asset, "future_receipt", "90", "2026-01-03T23:00:00Z", "2026-01-04T00:00:00Z"),
		mark(asset, "day3_bid", "38", "2026-01-03T18:30:00-05:00", "2026-01-03T23:40:00Z"),
		mark(asset, "day2_bid", "32", "2026-01-02T23:10:00Z", "2026-01-02T23:20:00Z"),
	}
	data := &history{events: []ledger.Event{
		create(t),
		order(t, 2, asset, "100", "2026-01-02T22:00:00Z"),
		fill(t, 3, 2, "partially_filled", "50", "15.00000005", "0.01", "2026-01-02T22:10:00Z", "2026-01-02T22:20:00Z"),
		fill(t, 4, 2, "filled", "", "15", "0.02", "2026-01-02T22:30:00Z", "2026-01-03T00:00:00Z"),
	}}
	got, err := Build(context.Background(), data, pinned(t, req), stamp("2026-01-05T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "incomplete" || got.NavBasis != "execution_net" || got.Funding.CashUSD != "1000" || len(got.DailyNAV) != 4 {
		t.Fatalf("unexpected report: %+v", got)
	}
	funding, partial, full, stale := got.DailyNAV[0], got.DailyNAV[1], got.DailyNAV[2], got.DailyNAV[3]
	if funding.AccountVersion != 1 || stringValue(funding.NAVUSD) != "1000.00000000" || len(funding.Positions) != 0 ||
		partial.AccountVersion != 3 || partial.CashUSD != "984.98999995" || stringValue(partial.NAVUSD) != "1000.98999995" ||
		partial.Positions[0].QuantityRaw != "50" || partial.Positions[0].Mark.ID != "day2_bid" ||
		full.AccountVersion != 4 || full.FeesUSD != "0.03" || stringValue(full.NAVUSD) != "1007.96999995" ||
		full.Positions[0].QuantityRaw != "100" || full.Positions[0].Mark.ID != "day3_bid" ||
		stale.Day != "2026-01-04" || stale.NAVUSD != nil || stale.Positions[0].Issue != "mark_stale" {
		t.Fatalf("unexpected valuation rows: %+v", got.DailyNAV)
	}
	if full.LedgerSHA256 != stale.LedgerSHA256 || full.LedgerSHA256 == partial.LedgerSHA256 ||
		len(data.requested) != 3 {
		t.Fatalf("projection did not replay the ledger once: %+v, requests %v", got.DailyNAV, data.requested)
	}
}

func TestReportRoundsTotalOnceAndRequiresExactRegistry(t *testing.T) {
	req := newRequest()
	req.EndDay = req.StartDay
	left := ledger.Instrument{ID: "stock:left", RegistryVersion: "v1", Decimals: 18}
	right := ledger.Instrument{ID: "stock:right", RegistryVersion: "v1", Decimals: 18}
	wrong := right
	wrong.RegistryVersion = "v2"
	req.Marks = []Mark{
		mark(left, "left", "1", "2026-01-01T23:30:00Z", "2026-01-01T23:30:00Z"),
		mark(wrong, "wrong", "500", "2026-01-01T23:30:00Z", "2026-01-01T23:30:00Z"),
	}
	h := &history{events: []ledger.Event{
		create(t),
		order(t, 2, left, "5000000000", "2026-01-01T20:00:00Z"),
		fill(t, 3, 2, "filled", "", "0.00000001", "0", "2026-01-01T20:00:00Z", "2026-01-01T21:00:00Z"),
		order(t, 4, right, "5000000000", "2026-01-01T21:05:00Z"),
		fill(t, 5, 4, "filled", "", "0.00000001", "0", "2026-01-01T21:05:00Z", "2026-01-01T21:08:00Z"),
	}}
	now := stamp("2026-01-02T00:00:00Z")
	got, err := Build(context.Background(), h, pinned(t, req), now)
	if err != nil || got.DailyNAV[0].NAVUSD != nil || got.DailyNAV[0].Positions[1].Issue != "mark_missing" {
		t.Fatalf("expected missing registry mark: %+v, %v", got, err)
	}
	req.Marks = append(req.Marks, mark(right, "right", "1", "2026-01-01T23:30:00Z", "2026-01-01T23:30:00Z"))
	got, err = Build(context.Background(), h, pinned(t, req), now)
	if err != nil || got.Status != "complete" || stringValue(got.DailyNAV[0].NAVUSD) != "999.99999999" {
		t.Fatalf("rounding per position changed exact total: %+v, %v", got, err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	validateReportSchema(t, raw)
}

func TestPinnedInputAndIncompleteUTCDate(t *testing.T) {
	req := newRequest()
	req.Marks = []Mark{}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	key := hex.EncodeToString(digest[:])
	if _, err := Parse(append(append([]byte{}, raw...), ' '), key); err == nil {
		t.Fatal("accepted altered manifest bytes")
	}
	if _, err := Parse(raw, key); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), &history{}, pinned(t, req), stamp("2026-01-04T23:59:59Z")); err == nil {
		t.Fatal("accepted an unfinished UTC day")
	}
	item := mark(asset, "same", "12", "2026-01-01T23:20:00Z", "2026-01-01T23:30:00Z")
	req.Marks = []Mark{item, item}
	raw, err = json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	digest = sha256.Sum256(raw)
	if _, err := Parse(raw, hex.EncodeToString(digest[:])); err == nil {
		t.Fatal("accepted duplicate mark identity")
	}
}
