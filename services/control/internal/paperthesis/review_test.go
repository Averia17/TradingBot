package paperthesis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/researchscheduler"
)

func fixtureID(i int) string { return fmt.Sprintf("a0000000-0000-0000-0000-%012d", i) }

type researchFixture struct {
	job    jobs.Job
	result jobs.Result
}

func (r researchFixture) Get(context.Context, string) (jobs.Job, error) { return r.job, nil }
func (r researchFixture) GetResult(context.Context, string) (jobs.Result, error) {
	return r.result, nil
}

type paperFixture struct {
	account ledger.Account
	order   ledger.Order
	events  []ledger.Event
}

func (p *paperFixture) GetAccount(context.Context, string) (ledger.Account, error) {
	return p.account, nil
}

func (p *paperFixture) GetOrder(context.Context, string, string) (ledger.Order, error) {
	return p.order, nil
}

func (p *paperFixture) Events(_ context.Context, _ string, after int64, limit int) ([]ledger.Event, error) {
	return p.events[after:min(int(after)+limit, len(p.events))], nil
}

func fixtureJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (p *paperFixture) add(t *testing.T, id, kind string, at time.Time, payload any) {
	t.Helper()
	p.account.Version++
	p.events = append(p.events, ledger.Event{ID: id, Kind: kind, AccountVersion: p.account.Version, RecordedAt: at, Payload: fixtureJSON(t, payload)})
}

func reviewedOpening(t *testing.T) (researchFixture, *paperFixture, time.Time) {
	t.Helper()
	opened := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC)
	created, completed := opened.Add(-time.Minute), opened.Add(-30*time.Second)
	account, order, job, decision := fixtureID(1), fixtureID(2), fixtureID(3), fixtureID(4)
	instrument := ledger.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 0}
	portfolio := contract.Portfolio{ID: fixtureID(5), AsOf: created, Currency: "USD", CashAsset: "USD", Cash: "1000", Reserved: "0", MaxPosition: "0.25", Positions: []contract.Position{}}
	confidence := 0.75
	reportHash := sha256.Sum256([]byte("test"))
	evidence := hex.EncodeToString(reportHash[:])
	batch := contract.Batch{
		Version: "0.1", Mode: "research", RunID: fixtureID(6), SnapshotID: portfolio.ID, AsOf: portfolio.AsOf, Created: created, Expires: opened.Add(time.Minute),
		Evidence: map[string]string{"TSLA:market_report": evidence, "TSLA:final_trade_decision": evidence},
		Intents: []contract.Intent{{ID: decision, Symbol: "TSLA", Action: "BUY", Notional: "100", Confidence: &confidence, Expected: &confidence,
			Horizon: contract.Horizon{Min: 2, Max: 5}, Thesis: []string{"fixture"}, Invalidation: []string{"fixture"}, Evidence: []string{"TSLA:market_report"}, CostBPS: 25}},
	}
	research := researchFixture{
		job: jobs.Job{ID: job, Status: "completed", CreatedAt: created, Request: fixtureJSON(t, map[string]any{
			"job_schema_version": "0.1", "job_id": job, "policy_version": "fixture-v1", "portfolio": portfolio, "requested_symbols": []string{"TSLA"},
			"data_manifest": map[string]string{"ref": "test.json", "sha256": strings.Repeat("b", 64)}, "max_llm_calls": 1,
		})},
		result: jobs.Result{JobID: job, Attempt: 1, CompletedAt: completed, Payload: fixtureJSON(t, map[string]any{
			"result_schema_version": "0.1", "batch": batch, "reports": map[string]any{"TSLA": map[string]string{"market_report": "test", "final_trade_decision": "test"}},
			"metadata": map[string]any{"source_mode": "synthetic_fixture", "alpha_measured": false, "llm_calls": 0},
		})},
	}
	request := ledger.ReservationRequest{AccountID: account, OrderID: order, ResearchJobID: job, DecisionID: decision, QuoteID: fixtureID(7), SnapshotVersion: 1,
		Instrument: instrument, Side: "BUY", QuantityRaw: "5", GrossUSD: "100", MaxFeeUSD: "1", ExpiresAt: opened.Add(time.Minute)}
	paper := &paperFixture{account: ledger.Account{ID: account, CashUSD: "940", ReservedCashUSD: "41", CreatedAt: created}, order: ledger.Order{ReservationRequest: request, CreatedAt: completed.Add(time.Second)}}
	paper.add(t, account, "paper_account.created", created, ledger.CreateAccount{ID: account, InitialCashUSD: "1000"})
	paper.add(t, order, "paper_order.reserved", paper.order.CreatedAt, request)
	paper.add(t, fixtureID(8), "paper_order.partially_filled", opened, ledger.ResolutionRequest{
		AccountID: account, OrderID: order, EventID: fixtureID(8), Outcome: "partially_filled", QuantityRaw: "3", GrossUSD: "60", FeeUSD: "0", ObservedAt: opened.Add(-time.Second),
	})
	paper.account.Positions = []ledger.Position{{Instrument: instrument, QuantityRaw: "3", ReservedQuantityRaw: "0"}}
	return research, paper, opened
}

func TestReviewStartsWithAvailablePartialAndRepeatsClockID(t *testing.T) {
	research, paper, opened := reviewedOpening(t)
	load := func(at time.Time) View {
		t.Helper()
		view, err := Load(context.Background(), research, paper, paper.account.ID, paper.order.OrderID, at)
		if err != nil {
			t.Fatal(err)
		}
		return view
	}
	first := load(opened.Add(time.Hour))
	if first.State != "open" || first.OpeningOrderState != "partially_filled" || first.OpenedAt == nil || !first.OpenedAt.Equal(opened) ||
		first.FirstFillEventID != fixtureID(8) || first.SourceQuantityRaw != "3" || first.ClockReview != nil ||
		first.NextReviewAt == nil || !first.NextReviewAt.Equal(opened.Add(2*time.Hour)) {
		t.Fatalf("unexpected initial review state: %+v", first)
	}
	due := load(opened.Add(2*time.Hour + time.Second))
	replay := load(opened.Add(3 * time.Hour))
	if due.ClockReview == nil || replay.ClockReview == nil || due.ClockReview.ID != replay.ClockReview.ID ||
		!due.ClockReview.At.Equal(opened.Add(2*time.Hour)) || !due.NextReviewAt.Equal(opened.Add(4*time.Hour)) {
		t.Fatalf("clock slot did not replay: %+v then %+v", due, replay)
	}
	expired := load(opened.Add(6 * time.Hour))
	if expired.State != "open" || expired.NextReviewAt != nil || expired.ClockReview == nil || expired.ClockReview.Reason != "expiry" ||
		!expired.ClockReview.At.Equal(opened.Add(5*time.Hour)) || expired.ClockReview.ID == due.ClockReview.ID {
		t.Fatalf("expiry must prompt review of still open units: %+v", expired)
	}
}

func TestReviewTracksReservedSaleCancellationAndClose(t *testing.T) {
	research, paper, opened := reviewedOpening(t)
	at := opened.Add(2 * time.Hour)
	load := func() View {
		t.Helper()
		view, err := Load(context.Background(), research, paper, paper.account.ID, paper.order.OrderID, at.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return view
	}
	sale := ledger.ReservationRequest{
		AccountID: paper.account.ID, OrderID: fixtureID(9), DecisionID: fixtureID(10), QuoteID: fixtureID(11), SnapshotVersion: 3,
		Instrument: paper.order.Instrument, Side: "SELL", QuantityRaw: "3", GrossUSD: "30", MaxFeeUSD: "1", ExpiresAt: at.Add(time.Minute),
	}
	paper.add(t, sale.OrderID, "paper_order.reserved", at, sale)
	paper.account.Positions[0].ReservedQuantityRaw = "3"
	reserved := load()
	if reserved.State != "open" || reserved.ReservedQuantityRaw != "3" || reserved.FreeQuantityRaw != "0" || reserved.SourceQuantityRaw != "3" {
		t.Fatalf("exit reservation changed thesis units: %+v", reserved)
	}
	cancel := ledger.ResolutionRequest{AccountID: sale.AccountID, OrderID: sale.OrderID, EventID: fixtureID(12), Outcome: "cancelled"}
	paper.add(t, cancel.EventID, "paper_order.cancelled", at.Add(time.Second), cancel)
	paper.account.Positions[0].ReservedQuantityRaw = "0"
	cancelled := load()
	if cancelled.State != "open" || cancelled.HeldQuantityRaw != "3" || cancelled.FreeQuantityRaw != "3" || cancelled.ClockReview.ID != reserved.ClockReview.ID {
		t.Fatalf("cancelled sale did not preserve opened thesis: %+v", cancelled)
	}
	sale.OrderID, sale.DecisionID, sale.QuoteID, sale.SnapshotVersion = fixtureID(13), fixtureID(14), fixtureID(15), paper.account.Version
	paper.add(t, sale.OrderID, "paper_order.reserved", at.Add(2*time.Second), sale)
	fill := ledger.ResolutionRequest{AccountID: sale.AccountID, OrderID: sale.OrderID, EventID: fixtureID(16), Outcome: "filled", GrossUSD: "30", FeeUSD: "0", ObservedAt: at.Add(3 * time.Second)}
	paper.add(t, fill.EventID, "paper_order.filled", at.Add(4*time.Second), fill)
	paper.account.Positions = nil
	paper.account.CashUSD = "970"
	closed := load()
	if closed.State != "closed" || closed.SourceQuantityRaw != "0" || closed.ClockReview != nil || closed.NextReviewAt != nil {
		t.Fatalf("closed units generated a review: %+v", closed)
	}
}

func TestReviewRejectsChangedSourceAndMixedVersions(t *testing.T) {
	research, paper, opened := reviewedOpening(t)
	load := func() error {
		_, err := Load(context.Background(), research, paper, paper.account.ID, paper.order.OrderID, opened)
		return err
	}
	paper.account.Version++
	if err := load(); !errors.Is(err, ledger.ErrStale) {
		t.Fatalf("mixed account version accepted: %v", err)
	}
	paper.account.Version--
	research.result.JobID = fixtureID(99)
	if err := load(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("swapped result accepted: %v", err)
	}
}

func TestReservedOrCancelledOpeningDoesNotStartReviewClock(t *testing.T) {
	research, paper, opened := reviewedOpening(t)
	paper.events = paper.events[:2]
	paper.account.Version, paper.account.CashUSD = 2, "1000"
	paper.account.ReservedCashUSD, paper.account.Positions = "101", nil
	load := func(expected string) {
		t.Helper()
		view, err := Load(context.Background(), research, paper, paper.account.ID, paper.order.OrderID, opened.Add(12*time.Hour))
		if err != nil || view.State != expected || view.OpenedAt != nil || view.ClockReview != nil || view.NextReviewAt != nil {
			t.Fatalf("unexecuted opening generated a review: %+v / %v", view, err)
		}
	}
	load("pending")
	cancel := ledger.ResolutionRequest{AccountID: paper.account.ID, OrderID: paper.order.OrderID, EventID: fixtureID(12), Outcome: "cancelled"}
	paper.add(t, cancel.EventID, "paper_order.cancelled", opened, cancel)
	paper.account.ReservedCashUSD = "0"
	load("unopened")
}

func TestThesisOwnsOnlyLotsFromExactOpeningFillEvents(t *testing.T) {
	research, paper, opened := reviewedOpening(t)
	additional := paper.order.ReservationRequest
	additional.OrderID, additional.QuoteID = fixtureID(31), fixtureID(32)
	additional.ResearchJobID, additional.SnapshotVersion = fixtureID(33), 3
	additional.QuantityRaw, additional.GrossUSD = "2", "40"
	paper.add(t, additional.OrderID, "paper_order.reserved", opened.Add(time.Second), additional)
	fill := ledger.ResolutionRequest{AccountID: paper.account.ID, OrderID: additional.OrderID, EventID: fixtureID(34), Outcome: "filled",
		GrossUSD: "40", FeeUSD: "0", ObservedAt: opened.Add(2 * time.Second)}
	paper.add(t, fill.EventID, "paper_order.filled", opened.Add(3*time.Second), fill)
	paper.account.Positions[0].QuantityRaw, paper.account.CashUSD = "5", "900"
	view, err := Load(context.Background(), research, paper, paper.account.ID, paper.order.OrderID, opened.Add(time.Minute))
	if err != nil || view.HeldQuantityRaw != "5" || view.SourceQuantityRaw != "3" {
		t.Fatalf("another order's decision ID mixed sourced lots: %+v / %v", view, err)
	}
}

func TestVerifiedCatalystCreatesRepeatedReviewForHeldSymbol(t *testing.T) {
	research, paper, opened := reviewedOpening(t)
	known, asOf := opened.Add(time.Minute), opened.Add(2*time.Minute)
	archive := []byte(fmt.Sprintf(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[{"kind":"news","provider":"fixture","record_id":"headline","symbol":"TSLA","revision":1,"event_time":%q,"available_at":%q,"ingested_at":%q,"payload":{"headline":"fixture"}}]}`,
		known.Format(time.RFC3339), known.Format(time.RFC3339), known.Format(time.RFC3339)))
	definition := fmt.Sprintf(`{"schema_version":"0.2","schedule_id":"paper-test","policy_version":"fixture-v1","max_lateness_seconds":120,"occurrences":[{"scheduled_at":%q,"event":{"kind":"news","provider":"fixture","record_id":"headline"},"portfolio":{"snapshot_id":%q,"as_of":%q,"currency":"USD","cash_asset":"USD","cash_usd":"940","reserved_cash_usd":"41","max_position_pct":"0.25","positions":[]},"requested_symbols":["TSLA"],"data_manifest":{"ref":"archive.json","sha256":"%x"},"max_llm_calls":1}]}`,
		asOf.Format(time.RFC3339), fixtureID(23), asOf.Format(time.RFC3339), sha256.Sum256(archive))
	schedule, err := researchscheduler.Load(strings.NewReader(definition))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "archive.json"), archive, 0600); err != nil {
		t.Fatal(err)
	}
	proof, err := schedule.VerifyEvent(root, "fixture", "headline")
	if err != nil {
		t.Fatal(err)
	}
	view, err := Load(context.Background(), research, paper, paper.account.ID, paper.order.OrderID, asOf)
	if err != nil {
		t.Fatal(err)
	}
	review, err := CatalystReview(view, proof)
	replayed, replayErr := CatalystReview(view, proof)
	if err != nil || replayErr != nil || review.ID == "" || review.ID != replayed.ID || review.Kind != "news" || !review.At.Equal(known) || view.ClockReview != nil {
		t.Fatalf("catalyst was not a stable early review: %+v / %v / %v", review, err, replayErr)
	}
	if _, err := CatalystReview(view, researchscheduler.VerifiedEvent{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unverified trigger accepted: %v", err)
	}
	view.Symbol = "NVDA"
	if _, err := CatalystReview(view, proof); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unrelated catalyst accepted: %v", err)
	}
}
