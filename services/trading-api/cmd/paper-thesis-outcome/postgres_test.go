package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/paper_thesis"
	"tradingbot/trading-api/internal/trading_api"
)

func TestReportCLIReadsReconciledFillsAndDecisionFromReadOnlyPostgres(t *testing.T) {
	base := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	dsn, err := url.Parse(base)
	if err != nil {
		t.Fatal("invalid test DB URL")
	}
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal("open test DB")
	}
	schema := "thesis_outcome_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer conn.Close(cleanup)
		if _, err := conn.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()

	store, err := accounts.Open(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	queue, err := jobs.Open(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()

	accountID, jobID := testUUID(1), testUUID(2)
	if _, created, err := store.CreateAccount(ctx, accounts.CreateAccount{ID: accountID, InitialCashUSD: "1000"}); err != nil || !created {
		t.Fatalf("create paper account: %v", err)
	}
	clock := func() time.Time {
		now, err := queue.Now(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return now
	}
	handler := trading_api.NewHandler(time.Hour, clock, queue)
	portfolio := contract.Portfolio{
		ID: testUUID(3), AsOf: clock().Add(-time.Second), Currency: "USD", CashAsset: "USD", Cash: "1000",
		Reserved: "0", MaxPosition: "0.25", Positions: []contract.Position{},
	}
	const jobsRoute = "/v1/research/jobs"
	post(t, handler, jobsRoute, map[string]any{
		"job_schema_version": "0.1", "job_id": jobID, "portfolio": portfolio,
		"requested_symbols": []string{"TSLA", "NVDA"}, "policy_version": "fixture-1",
		"data_manifest": map[string]string{"ref": "test.json", "sha256": strings.Repeat("b", 64)}, "max_llm_calls": 1,
	})
	lease, err := queue.Claim(ctx, "thesis-fixture", testUUID(4), time.Hour)
	if err != nil || lease.Job.ID != jobID {
		t.Fatalf("claim opening research: %v", err)
	}
	raw, err := os.ReadFile("../../../../contracts/valid-result-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var sourceFixture struct {
		Batch    contract.Batch  `json:"batch"`
		Reports  json.RawMessage `json:"reports"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &sourceFixture); err != nil {
		t.Fatal(err)
	}
	now := clock()
	batch := sourceFixture.Batch
	batch.AsOf, batch.Created, batch.Expires, batch.SnapshotID = portfolio.AsOf, now, now.Add(time.Minute), portfolio.ID
	batch.Intents[0].Action, batch.Intents[0].Notional = "BUY", "100"
	confidence, gain := 0.8, 5.0
	batch.Intents[0].Confidence, batch.Intents[0].Expected = &confidence, &gain
	post(t, handler, jobsRoute+"/"+jobID+"/complete", map[string]any{
		"result_schema_version": "0.1", "worker_id": lease.WorkerID, "lease_token": lease.Token,
		"batch": batch, "reports": sourceFixture.Reports, "metadata": sourceFixture.Metadata,
	})
	instrument := accounts.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 0}
	settle := func(id int, version int64, side, size, gross, fee, researchID, decisionID string) {
		t.Helper()
		order, created, err := store.Reserve(ctx, accounts.ReservationRequest{
			AccountID: accountID, OrderID: testUUID(id), DecisionID: decisionID,
			QuoteID: testUUID(id + 1), SnapshotVersion: version, ResearchJobID: researchID,
			Instrument: instrument, Side: side, QuantityRaw: size, GrossUSD: gross, MaxFeeUSD: fee,
			ExpiresAt: batch.Expires,
		})
		if err != nil || !created {
			t.Fatalf("reserve %s: %v", side, err)
		}
		if _, created, err := store.Resolve(ctx, accounts.ResolutionRequest{
			AccountID: accountID, OrderID: order.OrderID, EventID: testUUID(id + 2), Outcome: "filled",
			QuantityRaw: size, GrossUSD: gross, FeeUSD: fee, ObservedAt: order.CreatedAt,
		}); err != nil || !created {
			t.Fatalf("fill %s: %v", side, err)
		}
	}
	settle(10, 1, "BUY", "3", "60", "0", jobID, batch.Intents[0].ID)
	settle(20, 3, "SELL", "1", "24", "1", "", testUUID(23))
	req := paper_thesis.OutcomeRequest{
		Version: "0.1", Source: "synthetic_fixture", AccountID: accountID, OpeningOrderID: testUUID(10),
		AsOf: clock(), MaxObservationAgeSeconds: 120, ExitObservations: []paper_thesis.ExitObservation{},
	}
	raw, err = json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "outcome.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--input", path, "--sha256", fmt.Sprintf("%x", sha256.Sum256(raw))}
	query.Set("default_transaction_read_only", "on")
	dsn.RawQuery = query.Encode()
	var report, repeat bytes.Buffer
	lookup := func(ctx context.Context, id string) (jobs.CompletedPaperResearch, error) {
		return jobs.ReadCompletedPaperResearch(ctx, dsn.String(), id)
	}
	if err := run(ctx, dsn.String(), flags, &report, lookup); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, dsn.String(), flags, &repeat, lookup); err != nil || !bytes.Equal(report.Bytes(), repeat.Bytes()) {
		t.Fatalf("report is not reproducible: %v", err)
	}
	var value paper_thesis.OutcomeReport
	if err := contract.Decode(report.Bytes(), contract.PaperThesisOutcomeReport, &value); err != nil {
		t.Fatal(err)
	}
	if value.Status != "immature" || value.Actual.RealizedPnLUSD != "3" || value.Actual.OpenCostBasisUSD != "40" ||
		value.Actual.OpenQuantityRaw != "2" || value.AccountVersion != 5 || value.Horizon != nil || value.JobID != jobID {
		t.Fatalf("unexpected outcome: %s", report.Bytes())
	}
}

func testUUID(i int) string { return fmt.Sprintf("a0000000-0000-0000-0000-%012d", i) }

func post(t *testing.T, h http.Handler, path string, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("POST %s returned %d: %s", path, resp.Code, resp.Body)
	}
}
