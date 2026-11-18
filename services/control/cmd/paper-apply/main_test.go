package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperrisk"
	"tradingbot/control/internal/papersim"
)

func isolatedDatabase(t *testing.T) (context.Context, string, func() time.Time) {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("test database must be a PostgreSQL URI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "paper_apply_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer base.Close(cleanup)
		if _, err := base.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	params := parsed.Query()
	params.Set("search_path", schema)
	parsed.RawQuery = params.Encode()
	clock := func() time.Time {
		var now time.Time
		if err := base.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			t.Fatal("read test database clock failed")
		}
		return now.UTC()
	}
	return ctx, parsed.String(), clock
}

func encode(t *testing.T, value any) []byte {
	t.Helper()
	result, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func saveDocument(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func post(t *testing.T, handler http.Handler, path string, value any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encode(t, value)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: status %d, body %s", path, response.Code, response.Body.String())
	}
}

func validateReviewWire(t *testing.T, data []byte) {
	t.Helper()
	const root = "https://tradingbot.local/contracts/"
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	for _, file := range []string{"paper-thesis.schema.json", "paper-account.schema.json", "schema.json"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", file))
		if err != nil {
			t.Fatal(err)
		}
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(root+file, value); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile(root + "paper-thesis.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(payload); err != nil {
		t.Fatal(err)
	}
}

func TestPaperApplyCommandUsesDurableResearchAndSettlesOnlyOnce(t *testing.T) {
	const (
		accountID  = "11111111-1111-4111-8111-111111111111"
		jobID      = "22222222-2222-4222-8222-222222222222"
		decisionID = "33333333-3333-4333-8333-333333333333"
		runID      = "44444444-4444-4444-8444-444444444444"
		claimID    = "55555555-5555-4555-8555-555555555555"
		worker     = "paper-e2e"
		report     = "BUY from a synthetic test fixture"
		evidenceID = "TSLA:final_trade_decision"
	)
	ctx, dsn, clock := isolatedDatabase(t)
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer research.Close()
	ledgerStore, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer ledgerStore.Close()
	handler := control.NewHandlerWithPaper(time.Hour, clock, research, ledgerStore)
	post(t, handler, "/v1/paper/accounts", ledger.CreateAccount{ID: accountID, InitialCashUSD: "1000"})

	dir := t.TempDir()
	instrument := ledger.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 2}
	policy := paperrisk.Policy{
		Version: "paper-e2e", MaxOrderUSD: "120", MinFreeCashUSD: "100",
		MaxPositionBPS: 2500, MaxGroupBPS: 5000, MaxGrossBPS: 9000, MaxFeeBPS: 30, MaxMarkAgeSeconds: 60,
		Instruments: []paperrisk.InstrumentRule{{Instrument: instrument, Symbol: "TSLA", Group: "tech"}},
	}
	policyPath := saveDocument(t, dir, "policy", encode(t, policy))
	markAt := clock().Add(-time.Second)
	marksPath := saveDocument(t, dir, "marks", encode(t, []paperrisk.Mark{{Instrument: instrument, PriceUSD: "20", At: markAt, AvailableAt: markAt}}))
	snapshotArgs := []string{"snapshot", "--account", accountID, "--policy", policyPath, "--marks", marksPath}
	var output bytes.Buffer
	snapshotClock := func() time.Time { return clock().Add(time.Nanosecond) }
	if err := run(ctx, dsn, snapshotArgs, &output, snapshotClock); err != nil {
		t.Fatal(err)
	}
	var snapshot snapshotBundle
	if err := json.Unmarshal(output.Bytes(), &snapshot); err != nil || snapshot.Context.SnapshotVersion != 1 ||
		snapshot.Portfolio.Cash != "1000" || snapshot.Portfolio.AsOf.Nanosecond()%1000 != 0 {
		t.Fatalf("snapshot: %+v, %v", snapshot, err)
	}

	post(t, handler, "/v1/research/jobs", map[string]any{
		"job_schema_version": "0.1", "job_id": jobID, "portfolio": snapshot.Portfolio,
		"requested_symbols": []string{"TSLA"}, "policy_version": policy.Version, "max_llm_calls": 1,
		"data_manifest": map[string]string{"ref": "fixtures/paper.json", "sha256": strings.Repeat("a", 64)},
	})
	lease, err := research.Claim(ctx, worker, claimID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(report))
	createdAt := clock()
	batch := contract.Batch{
		Version: contract.BatchVersion, Mode: "research", RunID: runID, SnapshotID: snapshot.Portfolio.ID,
		AsOf: snapshot.Portfolio.AsOf, Created: createdAt, Expires: createdAt.Add(time.Minute),
		Evidence: map[string]string{evidenceID: hex.EncodeToString(digest[:])},
		Intents: []contract.Intent{{
			ID: decisionID, Symbol: "TSLA", Action: "BUY", Notional: "100", Confidence: new(0.75), Expected: new(2.0),
			Horizon: contract.Horizon{Min: 1, Max: 24}, Thesis: []string{"fixture"}, Invalidation: []string{"fixture"},
			Evidence: []string{evidenceID}, CostBPS: 30,
		}},
	}
	post(t, handler, "/v1/research/jobs/"+jobID+"/complete", map[string]any{
		"result_schema_version": "0.1", "worker_id": worker, "lease_token": lease.Token, "batch": batch,
		"reports":  map[string]map[string]string{"TSLA": {"final_trade_decision": report}},
		"metadata": map[string]any{"source_mode": "synthetic_fixture", "alpha_measured": false, "llm_calls": 0},
	})
	result, err := research.GetResult(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	quote := papersim.Quote{
		Version: papersim.Version, Source: papersim.Source, Instrument: instrument, Side: "BUY", QuantityRaw: "500",
		GrossUSD: "100", FeeBPS: new(20), FixedFeeUSD: "0", ObservedAt: result.CompletedAt.Add(time.Microsecond),
		ExpiresAt: batch.Expires, Execution: "fill",
	}
	quoteBytes := encode(t, quote)
	quotePath := saveDocument(t, dir, "quote", quoteBytes)
	quoteDigest := sha256.Sum256(quoteBytes)
	input := paperapply.Request{Version: paperapply.Version, JobID: jobID, SnapshotContext: snapshot.Context}
	requestPath := saveDocument(t, dir, "request", encode(t, input))
	applyArgs := []string{"apply", "--policy", policyPath, "--request", requestPath, "--quote", quotePath}
	output.Reset()
	if err := run(ctx, dsn, applyArgs, &output, clock); err != nil {
		t.Fatal(err)
	}
	var first paperapply.Result
	if err := json.Unmarshal(output.Bytes(), &first); err != nil || first.Execution == nil || !first.Execution.OutcomeRecorded ||
		first.Execution.QuoteSHA256 != hex.EncodeToString(quoteDigest[:]) ||
		first.Execution.Order.DecisionID != decisionID || first.Execution.Order.ResearchJobID != jobID || first.Execution.Order.Status != "filled" {
		t.Fatalf("application receipt: %+v, %v", first, err)
	}
	output.Reset()
	if err := run(ctx, dsn, applyArgs, &output, func() time.Time { return batch.Expires.Add(time.Minute) }); err != nil {
		t.Fatal(err)
	}
	var replay paperapply.Result
	if err := json.Unmarshal(output.Bytes(), &replay); err != nil || replay.Execution == nil || replay.Execution.ReservationRecorded ||
		replay.Execution.OutcomeRecorded || replay.Execution.Order.Fill.EventID != first.Execution.Order.Fill.EventID {
		t.Fatalf("application replay: %+v, %v", replay, err)
	}
	account, err := ledgerStore.GetAccount(ctx, accountID)
	if err != nil || account.Version != 3 || account.CashUSD != "899.8" || account.ReservedCashUSD != "0" ||
		len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "500" {
		t.Fatalf("unexpected account after replay: %+v, %v", account, err)
	}
	output.Reset()
	reviewArgs := []string{"review", "--account", accountID, "--order", first.Execution.Order.OrderID}
	if err := run(ctx, dsn, reviewArgs, &output, clock); err != nil {
		t.Fatal(err)
	}
	var current reviewReceipt
	if err := json.Unmarshal(output.Bytes(), &current); err != nil || current.State != "open" || current.JobID != jobID ||
		current.SourceQuantityRaw != "500" || current.FreeQuantityRaw != "500" || current.OpenedAt == nil ||
		current.NextReviewAt == nil || current.ClockReview != nil || !current.NextReviewAt.Equal(current.OpenedAt.Add(time.Hour)) {
		t.Fatalf("thesis review: %+v, %v", current, err)
	}
	output.Reset()
	due := current.NextReviewAt.Add(time.Second)
	if err := run(ctx, dsn, reviewArgs, &output, func() time.Time { return due }); err != nil {
		t.Fatal(err)
	}
	var triggered reviewReceipt
	if err := json.Unmarshal(output.Bytes(), &triggered); err != nil || triggered.ClockReview == nil ||
		triggered.ClockReview.ID == "" || !triggered.ClockReview.At.Equal(*current.NextReviewAt) {
		t.Fatalf("due thesis review: %+v, %v", triggered, err)
	}
	validateReviewWire(t, output.Bytes())
	backdated := append(snapshotArgs, "--as-of", snapshot.Portfolio.AsOf.Format(time.RFC3339Nano))
	if err := run(ctx, dsn, backdated, &output, clock); err == nil {
		t.Fatal("snapshot exposed a later account version at an old cutoff")
	}
	policy.MaxOrderUSD = "110"
	changed := saveDocument(t, dir, "changed-policy", encode(t, policy))
	if err := run(ctx, dsn, []string{"apply", "--policy", changed, "--request", requestPath, "--quote", quotePath}, &output, clock); err == nil {
		t.Fatal("application accepted a request with a different owner policy")
	}
}
