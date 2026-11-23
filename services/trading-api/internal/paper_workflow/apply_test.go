package paper_workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/paper_risk"
	"tradingbot/trading-api/internal/paper_sim"
)

const (
	accountID  = "00000000-0000-0000-0000-000000000001"
	jobID      = "00000000-0000-0000-0000-000000000002"
	decisionID = "00000000-0000-0000-0000-000000000003"
	runID      = "00000000-0000-0000-0000-000000000004"
)

type researchStub struct {
	job    jobs.Job
	result jobs.Result
}

func (s researchStub) Get(context.Context, string) (jobs.Job, error)          { return s.job, nil }
func (s researchStub) GetResult(context.Context, string) (jobs.Result, error) { return s.result, nil }

type ledgerStub struct {
	account     accounts.Account
	order       accounts.Order
	reserves    int
	resolutions int
	resolveErr  error
}

func (s *ledgerStub) GetAccount(context.Context, string) (accounts.Account, error) {
	return s.account, nil
}
func (s *ledgerStub) GetOrder(_ context.Context, _, id string) (accounts.Order, error) {
	if s.order.OrderID != id {
		return accounts.Order{}, accounts.ErrNotFound
	}
	return s.order, nil
}
func (s *ledgerStub) RecordRiskRefusal(context.Context, accounts.RiskRefusalInput) error { return nil }
func (s *ledgerStub) Reserve(_ context.Context, req accounts.ReservationRequest) (accounts.Order, bool, error) {
	s.reserves++
	if s.order.OrderID == req.OrderID {
		return s.order, false, nil
	}
	if s.account.Version != req.SnapshotVersion {
		return accounts.Order{}, false, accounts.ErrStale
	}
	s.account.Version++
	s.order = accounts.Order{ReservationRequest: req, Status: "reserved", AccountVersion: s.account.Version, CreatedAt: req.ExpiresAt.Add(-time.Second)}
	return s.order, true, nil
}
func (s *ledgerStub) Resolve(_ context.Context, req accounts.ResolutionRequest) (accounts.Order, bool, error) {
	s.resolutions++
	if s.resolveErr != nil {
		return accounts.Order{}, false, s.resolveErr
	}
	if s.order.Fill != nil {
		return s.order, false, nil
	}
	s.account.Version++
	s.order.Status = req.Outcome
	s.order.AccountVersion = s.account.Version
	s.order.Fill = &accounts.Fill{EventID: req.EventID, GrossUSD: req.GrossUSD, FeeUSD: req.FeeUSD, ObservedAt: req.ObservedAt}
	return s.order, true, nil
}

func marshalFixture(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func approveFixture(t *testing.T, hold bool) (researchStub, *ledgerStub, Request, []byte, time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	asOf := now.Add(-3 * time.Second)
	completed := now.Add(-2 * time.Second)
	deadline := now.Add(time.Minute)
	instrument := accounts.Instrument{ID: "fixture:TSLA", RegistryVersion: "test-v1", Decimals: 2}
	context := SnapshotContext{AccountID: accountID, SnapshotVersion: 1,
		Policy: paper_risk.Policy{Version: "paper-test", MaxOrderUSD: "150", MinFreeCashUSD: "100", MaxPositionBPS: 2500, MaxGroupBPS: 5000, MaxGrossBPS: 9000, MaxFeeBPS: 30, MaxMarkAgeSeconds: 60,
			Instruments: []paper_risk.InstrumentRule{{Instrument: instrument, Symbol: "TSLA", Group: "tech"}}},
		Marks: []paper_risk.Mark{{Instrument: instrument, PriceUSD: "20", At: asOf, AvailableAt: asOf}},
	}
	ledger := &ledgerStub{account: accounts.Account{ID: accountID, Version: 1, CashUSD: "1000", ReservedCashUSD: "0", Positions: []accounts.Position{}, CreatedAt: asOf.Add(-time.Minute)}}
	portfolio, err := Snapshot(ledger.account, context, asOf)
	if err != nil {
		t.Fatal(err)
	}
	confidence := 0.75
	proposal := contract.Intent{ID: decisionID, Symbol: "TSLA", Action: "BUY", Notional: "100", Confidence: &confidence, Expected: &confidence,
		Horizon: contract.Horizon{Min: 1, Max: 24}, Thesis: []string{"fixture"}, Invalidation: []string{"fixture"}, Evidence: []string{"TSLA:market_report"}, CostBPS: 25}
	if hold {
		proposal.Action, proposal.Notional, proposal.Confidence, proposal.Expected = "HOLD", "0", nil, nil
	}
	report := sha256.Sum256([]byte("fixture"))
	batch := contract.Batch{Version: "0.1", Mode: "research", RunID: runID, SnapshotID: portfolio.ID, AsOf: asOf, Created: asOf, Expires: deadline,
		Evidence: map[string]string{"TSLA:market_report": hex.EncodeToString(report[:]), "TSLA:final_trade_decision": hex.EncodeToString(report[:])}, Intents: []contract.Intent{proposal}}
	job := jobs.Job{ID: jobID, Status: "completed", CreatedAt: asOf, Request: marshalFixture(t, map[string]any{
		"job_schema_version": "0.1", "job_id": jobID, "policy_version": context.Policy.Version, "portfolio": portfolio,
		"requested_symbols": []string{"TSLA"}, "data_manifest": map[string]string{"ref": "test/manifest.json", "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, "max_llm_calls": 1,
	})}
	result := jobs.Result{JobID: jobID, Attempt: 1, CompletedAt: completed, Payload: marshalFixture(t, map[string]any{
		"result_schema_version": "0.1", "batch": batch, "reports": map[string]map[string]string{"TSLA": {"market_report": "fixture", "final_trade_decision": "fixture"}},
		"metadata": map[string]any{"source_mode": "synthetic_fixture", "alpha_measured": false, "llm_calls": 0},
	})}
	fee := 20
	quote := paper_sim.Quote{Version: "0.1", Source: "synthetic_fixture", Instrument: instrument, Side: "BUY", QuantityRaw: "500", GrossUSD: "100", FeeBPS: &fee, FixedFeeUSD: "0", ObservedAt: now.Add(-time.Second), ExpiresAt: deadline, Execution: "fill"}
	bytes := marshalFixture(t, quote)
	digest := sha256.Sum256(bytes)
	intent := &paper_sim.Intent{Version: "0.1", AccountID: accountID, DecisionID: decisionID, SnapshotVersion: 1, Instrument: instrument, Side: "BUY", QuantityRaw: "500",
		GrossLimitUSD: "100", MaxFeeUSD: "0.25", DecidedAt: completed, ExpiresAt: deadline, QuoteSHA256: hex.EncodeToString(digest[:])}
	if hold {
		intent, bytes = nil, nil
	}
	return researchStub{job, result}, ledger, Request{Version: "0.1", JobID: jobID, SnapshotContext: context, Intent: intent}, bytes, now
}

func TestApplyCommitsAndReplaysAfterStateAdvanceAndExpiry(t *testing.T) {
	research, ledger, req, quote, now := approveFixture(t, false)
	first, err := Apply(context.Background(), research, ledger, req, quote, now)
	if err != nil || first.Execution == nil || first.Execution.Order.Status != "filled" || !first.Execution.OutcomeRecorded {
		t.Fatalf("apply: %+v, %v", first, err)
	}
	ledger.account.CashUSD = "899.8"
	replayed, err := Apply(context.Background(), research, ledger, req, quote, now.Add(time.Hour))
	if err != nil || replayed.Execution == nil || replayed.Execution.Order.Fill.EventID != first.Execution.Order.Fill.EventID || replayed.Execution.OutcomeRecorded {
		t.Fatalf("replay: %+v, %v", replayed, err)
	}
	if ledger.account.Version != 3 {
		t.Fatalf("duplicate execution changed account: %d", ledger.account.Version)
	}
}

func TestApplyRejectsSwappedPolicyStateQuoteAndExpiredFreshOrder(t *testing.T) {
	for _, name := range []string{"policy", "cash", "quantity", "quote", "expire", "risk"} {
		t.Run(name, func(t *testing.T) {
			research, ledger, req, quote, now := approveFixture(t, false)
			switch name {
			case "policy":
				req.SnapshotContext.Policy.MaxOrderUSD = "200"
			case "cash":
				ledger.account.CashUSD = "1001"
			case "quantity":
				req.Intent.QuantityRaw = "600"
			case "quote":
				quote = append(quote, '\n')
			case "expire":
				now = now.Add(time.Hour)
			case "risk":
				req.Intent.MaxFeeUSD = "0.5"
			}
			if _, err := Apply(context.Background(), research, ledger, req, quote, now); err == nil || ledger.reserves != 0 {
				t.Fatalf("expected pre-reservation rejection; err=%v reserves=%d", err, ledger.reserves)
			}
		})
	}
}

func TestApplyHoldNeverWritesOrders(t *testing.T) {
	research, ledger, req, quote, now := approveFixture(t, true)
	result, err := Apply(context.Background(), research, ledger, req, quote, now)
	if err != nil || result.Status != "hold" || result.Execution != nil || ledger.reserves != 0 {
		t.Fatalf("hold: %+v, %v", result, err)
	}
	research.job.Status = "leased"
	if _, err := Apply(context.Background(), research, ledger, req, quote, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("leased result accepted: %v", err)
	}
}

func TestApplyReconcilesCommittedReservationAfterFailureAndExpiry(t *testing.T) {
	research, paper, req, quote, now := approveFixture(t, false)
	paper.resolveErr = errors.New("synthetic resolution unavailable")
	if _, err := Apply(context.Background(), research, paper, req, quote, now); !errors.Is(err, paper.resolveErr) || paper.order.Status != "reserved" {
		t.Fatalf("expected a retained reservation, got %+v / %v", paper.order, err)
	}
	paper.resolveErr = nil
	result, err := Apply(context.Background(), research, paper, req, quote, now.Add(time.Hour))
	if err != nil || result.Execution == nil || result.Execution.ReservationRecorded || !result.Execution.OutcomeRecorded || result.Execution.Order.Status != "filled" {
		t.Fatalf("reconcile existing reservation: %+v / %v", result, err)
	}
	if paper.account.Version != 3 || paper.reserves != 2 || paper.resolutions != 2 {
		t.Fatalf("expected one economic reservation and fill: %+v", paper)
	}
}

func TestApplyRejectsMismatchedEvidenceAndUnsupportedSource(t *testing.T) {
	for _, field := range []string{"reports", "metadata"} {
		t.Run(field, func(t *testing.T) {
			research, paper, req, quote, now := approveFixture(t, false)
			var document storedResult
			if err := json.Unmarshal(research.result.Payload, &document); err != nil {
				t.Fatal(err)
			}
			if field == "reports" {
				document.Reports["TSLA"]["market_report"] = "changed evidence"
			} else {
				document.Metadata = json.RawMessage(`{"source_mode":"hypothetical_live","alpha_measured":false,"llm_calls":0}`)
			}
			research.result.Payload = marshalFixture(t, document)
			if _, err := Apply(context.Background(), research, paper, req, quote, now); !errors.Is(err, ErrInvalid) || paper.reserves != 0 {
				t.Fatalf("invalid receipt accepted: %v, %d reservations", err, paper.reserves)
			}
		})
	}
}

func TestApplyRejectsCostAboveDecisionAndExpiredQuoteBeforeReserve(t *testing.T) {
	for _, test := range []struct {
		name string
		want error
	}{{"quote expired", accounts.ErrExpired}, {"adverse gross", ErrInvalid}} {
		t.Run(test.name, func(t *testing.T) {
			research, paper, req, raw, now := approveFixture(t, false)
			var quote paper_sim.Quote
			if err := json.Unmarshal(raw, &quote); err != nil {
				t.Fatal(err)
			}
			if test.name == "quote expired" {
				quote.ExpiresAt = now
			} else {
				// At the $20 mark, these units are worth $98. The $100 quote
				// satisfies the limit but exceeds the decision's all-in cap.
				req.Intent.QuantityRaw, quote.QuantityRaw = "490", "490"
				req.Intent.MaxFeeUSD = "0.24"
			}
			raw = marshalFixture(t, quote)
			digest := sha256.Sum256(raw)
			req.Intent.QuoteSHA256 = hex.EncodeToString(digest[:])
			if _, err := Apply(context.Background(), research, paper, req, raw, now); !errors.Is(err, test.want) || paper.reserves != 0 {
				t.Fatalf("invalid quote accepted: %v, %d reservations", err, paper.reserves)
			}
		})
	}
}

func TestApplyRejectsPartialWhenFixedFeeConsumesTheExecutedSize(t *testing.T) {
	research, paper, req, raw, now := approveFixture(t, false)
	var quote paper_sim.Quote
	if err := json.Unmarshal(raw, &quote); err != nil {
		t.Fatal(err)
	}
	quote.Version, quote.Execution, quote.ExecutedQuantityRaw = paper_sim.PartialVersion, "partial", "100"
	quote.FeeBPS, quote.FixedFeeUSD = new(0), "0.19"
	req.Intent = nil // Infer size and limits from the quote against the saved decision.
	if _, err := Apply(context.Background(), research, paper, req, marshalFixture(t, quote), now); !errors.Is(err, ErrInvalid) ||
		paper.reserves != 0 {
		t.Fatalf("fixed fee above the $20 slice's cap was accepted: %v, %d reservations", err, paper.reserves)
	}
}

func TestSnapshotRejectsPositionPrecisionLossAndFutureMarks(t *testing.T) {
	_, paper, req, _, now := approveFixture(t, false)
	config := req.SnapshotContext
	instrument := config.Policy.Instruments[0].Instrument
	instrument.Decimals = 12
	config.Policy.Instruments[0].Instrument = instrument
	config.Marks[0].Instrument = instrument
	paper.account.Positions = []accounts.Position{{Instrument: instrument, QuantityRaw: "1000000000001", ReservedQuantityRaw: "0"}}
	if _, err := Snapshot(paper.account, config, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("research snapshot silently rounded: %v", err)
	}
	paper.account.Positions[0].QuantityRaw = "1000000010000"
	portfolio, err := Snapshot(paper.account, config, now)
	if err != nil || portfolio.Positions[0].Quantity != "1.00000001" {
		t.Fatalf("exact position: %+v / %v", portfolio, err)
	}
	config.Marks[0].AvailableAt = now.Add(time.Nanosecond)
	if _, err := Snapshot(paper.account, config, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("research snapshot used future mark: %v", err)
	}
}

func TestApplyAuthorizedSellAndReductionUseExactHeldUnits(t *testing.T) {
	for _, test := range []struct {
		name, action, notional, quantity, gross string
		ok                                      bool
	}{
		{"full sale", "SELL", "100", "500", "100", true},
		{"partial reduction", "REDUCE", "40", "200", "40", true},
		{"shorter than full sale", "SELL", "100", "200", "40", false},
		{"larger than reduction", "REDUCE", "40", "500", "100", false},
		{"gross above order policy", "SELL", "100", "500", "151", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			research, paper, req, raw, now := approveFixture(t, false)
			paper.account.Positions = []accounts.Position{{Instrument: req.Intent.Instrument, QuantityRaw: "500", ReservedQuantityRaw: "0"}}
			var request storedJob
			var document storedResult
			var batch contract.Batch
			var quote paper_sim.Quote
			for _, decode := range []struct {
				body []byte
				out  any
			}{{research.job.Request, &request}, {research.result.Payload, &document}, {raw, &quote}} {
				if err := json.Unmarshal(decode.body, decode.out); err != nil {
					t.Fatal(err)
				}
			}
			if err := json.Unmarshal(document.Batch, &batch); err != nil {
				t.Fatal(err)
			}
			portfolio, err := Snapshot(paper.account, req.SnapshotContext, batch.AsOf)
			if err != nil {
				t.Fatal(err)
			}
			request.Portfolio = marshalFixture(t, portfolio)
			batch.SnapshotID = portfolio.ID
			batch.Intents[0].Action, batch.Intents[0].Notional = test.action, test.notional
			document.Batch = marshalFixture(t, batch)
			research.job.Request = marshalFixture(t, request)
			research.result.Payload = marshalFixture(t, document)
			quote.Side, quote.QuantityRaw, quote.GrossUSD = "SELL", test.quantity, test.gross
			fee := 0
			quote.FeeBPS = &fee
			req.Intent.Side, req.Intent.QuantityRaw, req.Intent.GrossLimitUSD, req.Intent.MaxFeeUSD = "SELL", test.quantity, test.notional, "0"
			raw = marshalFixture(t, quote)
			digest := sha256.Sum256(raw)
			req.Intent.QuoteSHA256 = hex.EncodeToString(digest[:])
			result, err := Apply(context.Background(), research, paper, req, raw, now)
			if test.ok {
				if err != nil || result.Execution == nil || result.Execution.Order.Side != "SELL" {
					t.Fatalf("sale rejected: %+v, %v", result, err)
				}
			} else if !errors.Is(err, ErrInvalid) || paper.reserves != 0 {
				t.Fatalf("unexpected sale: %+v, %v with %d reservations", result, err, paper.reserves)
			}
		})
	}
}
