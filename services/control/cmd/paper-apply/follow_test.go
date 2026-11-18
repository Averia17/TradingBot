package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperrisk"
	"tradingbot/control/internal/papersim"
)

func TestFollowPinsQuoteAndRecoversCommittedReservationFromPostgres(t *testing.T) {
	t.Run("fill", func(t *testing.T) {
		testFollowRecovery(t, "fill", "filled")
	})
	t.Run("timeout", func(t *testing.T) {
		testFollowRecovery(t, "timeout", "unknown")
	})
}

func testFollowRecovery(t *testing.T, quoteOutcome, orderStatus string) {
	t.Helper()
	const (
		accountID  = "11111111-1111-4111-8111-111111111111"
		jobID      = "22222222-2222-4222-8222-222222222222"
		decisionID = "33333333-3333-4333-8333-333333333333"
		runID      = "44444444-4444-4444-8444-444444444444"
		claimID    = "55555555-5555-4555-8555-555555555555"
	)
	ctx, dsn, clock := isolatedDatabase(t)
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer research.Close()
	paper, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer paper.Close()
	api := control.NewHandlerWithPaper(time.Hour, clock, research, paper)
	post(t, api, "/v1/paper/accounts", ledger.CreateAccount{ID: accountID, InitialCashUSD: "1000"})
	account, err := paper.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	instrument := ledger.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 2}
	policy := paperrisk.Policy{
		Version: "follow-fixture", MaxOrderUSD: "120", MinFreeCashUSD: "100", MaxPositionBPS: 2500, MaxGroupBPS: 5000,
		MaxGrossBPS: 9000, MaxFeeBPS: 30, MaxMarkAgeSeconds: 60, MaxDailyTurnoverUSD: "200", BlockNewBuysOnUnknown: true,
		Instruments: []paperrisk.InstrumentRule{{Instrument: instrument, Symbol: "TSLA", Group: "tech"}},
	}
	at := clock()
	snapshot := paperapply.SnapshotContext{AccountID: accountID, SnapshotVersion: account.Version, Policy: policy,
		Marks: []paperrisk.Mark{{Instrument: instrument, PriceUSD: "20", At: at, AvailableAt: at}}}
	portfolio, err := paperapply.Snapshot(account, snapshot, at)
	if err != nil {
		t.Fatal(err)
	}
	input := paperapply.Request{Version: paperapply.Version, JobID: jobID, SnapshotContext: snapshot}
	dir := t.TempDir()
	policyPath := saveDocument(t, dir, "policy", encode(t, policy))
	requestPath := saveDocument(t, dir, "request", encode(t, input))
	args := []string{"follow", "--policy", policyPath, "--request", requestPath, "--quote-root", dir}
	pass := func(want string) paperapply.FollowResult {
		t.Helper()
		var output bytes.Buffer
		if err := run(ctx, dsn, args, &output, clock); err != nil {
			t.Fatal(err)
		}
		var result paperapply.FollowResult
		if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.JobID != jobID || result.Status != want {
			t.Fatalf("follow receipt %s: %s, %v", want, output.Bytes(), err)
		}
		return result
	}
	post(t, api, "/v1/research/jobs", map[string]any{
		"job_schema_version": "0.1", "job_id": jobID, "portfolio": portfolio, "requested_symbols": []string{"TSLA"},
		"policy_version": policy.Version, "max_llm_calls": 1, "data_manifest": map[string]string{
			"ref": "follow.json", "sha256": strings.Repeat("a", 64),
		},
	})
	pass("awaiting_research")
	lease, err := research.Claim(ctx, "follow-fixture", claimID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const report = "BUY from a synthetic test fixture"
	const evidenceID = "TSLA:final_trade_decision"
	digest := sha256.Sum256([]byte(report))
	created := clock()
	batch := contract.Batch{
		Version: contract.BatchVersion, Mode: "research", RunID: runID, SnapshotID: portfolio.ID, AsOf: portfolio.AsOf,
		Created: created, Expires: created.Add(time.Minute), Evidence: map[string]string{evidenceID: hex.EncodeToString(digest[:])},
		Intents: []contract.Intent{{ID: decisionID, Symbol: "TSLA", Action: "BUY", Notional: "100", Confidence: new(0.75),
			Expected: new(2.0), Horizon: contract.Horizon{Min: 1, Max: 24}, Thesis: []string{"fixture"},
			Invalidation: []string{"fixture"}, Evidence: []string{evidenceID}, CostBPS: 30}},
	}
	post(t, api, "/v1/research/jobs/"+jobID+"/complete", map[string]any{
		"result_schema_version": "0.1", "worker_id": lease.WorkerID, "lease_token": lease.Token, "batch": batch,
		"reports":  map[string]map[string]string{"TSLA": {"final_trade_decision": report}},
		"metadata": map[string]any{"source_mode": "synthetic_fixture", "alpha_measured": false, "llm_calls": 0},
	})
	pass("awaiting_quote")
	completed, err := research.GetResult(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	quote := papersim.Quote{
		Version: papersim.Version, Source: papersim.Source, Instrument: instrument, Side: "BUY", QuantityRaw: "500",
		GrossUSD: "100", FeeBPS: new(20), FixedFeeUSD: "0", ObservedAt: completed.CompletedAt.Add(time.Microsecond),
		ExpiresAt: completed.CompletedAt.Add(2 * time.Microsecond), Execution: quoteOutcome,
	}
	publish := func(value papersim.Quote) string {
		t.Helper()
		raw := encode(t, value)
		quoteDigest := sha256.Sum256(raw)
		path := saveDocument(t, dir, "quote", raw)
		saveDocument(t, dir, jobID, encode(t, map[string]any{
			"paper_quote_pin_schema_version": "0.1", "job_id": jobID,
			"quote": map[string]string{"ref": "quote.json", "sha256": hex.EncodeToString(quoteDigest[:])},
		}))
		return path
	}
	publish(quote)
	pass("quote_expired")
	quote.ExpiresAt = batch.Expires
	path := publish(quote)
	if err := os.WriteFile(path, append(encode(t, quote), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, dsn, args, &bytes.Buffer{}, clock); err == nil || !strings.Contains(err.Error(), "pinned SHA-256") {
		t.Fatalf("tampered file: %v", err)
	}
	if _, err := paper.GetApplication(ctx, jobID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("untrusted quote was persisted: %v", err)
	}
	publish(quote)
	disrupted := errors.New("simulated interruption after reservation")
	_, err = paperapply.Follow(ctx, research, interruptedFollowStore{Store: paper, err: disrupted}, input, localQuoteSource(dir), clock())
	if !errors.Is(err, disrupted) {
		t.Fatalf("expected interrupted reconciliation: %v", err)
	}
	pinned, err := paper.GetApplication(ctx, jobID)
	if err != nil || pinned.OrderID == "" || len(pinned.Result) != 0 || !bytes.Equal(pinned.Quote, encode(t, quote)) {
		t.Fatalf("quote did not survive reserve: %+v, %v", pinned, err)
	}
	waiting, err := paper.GetAccount(ctx, accountID)
	if err != nil || waiting.Version != 2 || waiting.CashUSD != "1000" || waiting.ReservedCashUSD != "100.2" {
		t.Fatalf("pending reservation: %+v, %v", waiting, err)
	}
	if _, err := paper.RecordApplication(ctx, pinned.ApplicationInput, pinned.OrderID, []byte("{}"), batch.Expires); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("acknowledged a still unresolved application: %v", err)
	}
	args[len(args)-1] = filepath.Join(dir, "missing-source")
	if quoteOutcome == "timeout" {
		// A restart after the quote and model result deadlines may resolve an
		// already reserved order once. Unknown still retains the reservation.
		result, err := paperapply.Follow(ctx, research, paper, input, nil, batch.Expires.Add(time.Second))
		if err != nil || result.Application == nil || result.Application.Execution == nil ||
			result.Application.Execution.Order.Status != orderStatus {
			t.Fatalf("recover timed out quote: %+v, %v", result, err)
		}
	}
	recovered := pass("applied")
	if recovered.Application == nil || recovered.Application.Execution == nil ||
		recovered.Application.Execution.Order.OrderID != pinned.OrderID || recovered.Application.Execution.Order.Status != orderStatus {
		t.Fatalf("recovered receipt: %+v", recovered)
	}
	if replayed := pass("applied"); !bytes.Equal(encode(t, recovered), encode(t, replayed)) {
		t.Fatal("recorded application did not replay exactly")
	}
	settled, err := paper.GetAccount(ctx, accountID)
	if err != nil || settled.Version != 3 {
		t.Fatalf("recovered ledger: %+v, %v", settled, err)
	}
	if quoteOutcome == "fill" {
		if settled.CashUSD != "899.8" || settled.ReservedCashUSD != "0" ||
			len(settled.Positions) != 1 || settled.Positions[0].QuantityRaw != "500" {
			t.Fatalf("fill accounted incorrectly: %+v", settled)
		}
	} else if settled.CashUSD != "1000" || settled.ReservedCashUSD != "100.2" || len(settled.Positions) != 0 {
		t.Fatalf("timeout lost its reservation or fabricated a fill: %+v", settled)
	}
	input.SnapshotContext.Marks[0].PriceUSD = "21"
	saveDocument(t, dir, "request", encode(t, input))
	if err := run(ctx, dsn, args, &bytes.Buffer{}, clock); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("changed application request: %v", err)
	}
}

type interruptedFollowStore struct {
	*ledger.Store
	err error
}

func (s interruptedFollowStore) Resolve(context.Context, ledger.ResolutionRequest) (ledger.Order, bool, error) {
	return ledger.Order{}, false, s.err
}
