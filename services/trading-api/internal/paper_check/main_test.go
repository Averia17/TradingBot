package paper_check

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/jobs"
)

func TestPaperCheckReportsPostgresAnomalies(t *testing.T) {
	ctx, dsn, inspection := diagnosticDatabase(t)
	paper, err := accounts.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paper.Close)
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(research.Close)
	const accountID = "00000000-0000-4000-8000-000000000001"
	const jobID = "00000000-0000-4000-8000-000000000002"
	const snapshotID = "00000000-0000-4000-8000-000000000003"
	if _, _, err := paper.CreateAccount(ctx, accounts.CreateAccount{ID: accountID, InitialCashUSD: "9007199254740993.12345678"}); err != nil {
		t.Fatal(err)
	}
	reserve := accounts.ReservationRequest{
		AccountID: accountID, OrderID: "00000000-0000-4000-8000-000000000004",
		DecisionID: "00000000-0000-4000-8000-000000000005", QuoteID: "00000000-0000-4000-8000-000000000006",
		Instrument: accounts.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 8},
		Side:       "BUY", QuantityRaw: "100000000000000000001", GrossUSD: "20.12345678", MaxFeeUSD: "0.00000001",
		SnapshotVersion: 1, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	order, _, err := paper.Reserve(ctx, reserve)
	if err != nil {
		t.Fatal(err)
	}
	fill := accounts.ResolutionRequest{
		AccountID: accountID, OrderID: reserve.OrderID, EventID: "00000000-0000-4000-8000-000000000007",
		Outcome: "filled", GrossUSD: reserve.GrossUSD, FeeUSD: reserve.MaxFeeUSD, ObservedAt: order.CreatedAt,
	}
	if _, _, err := paper.Resolve(ctx, fill); err != nil {
		t.Fatal(err)
	}
	pending := reserve
	pending.SnapshotVersion = 3
	pending.OrderID = "00000000-0000-4000-8000-000000000008"
	pending.DecisionID = "00000000-0000-4000-8000-000000000009"
	pending.QuoteID = "00000000-0000-4000-8000-000000000010"
	if _, _, err := paper.Reserve(ctx, pending); err != nil {
		t.Fatal(err)
	}
	const unknownID = "00000000-0000-4000-8000-000000000011"
	if _, _, err := paper.Resolve(ctx, accounts.ResolutionRequest{AccountID: accountID, OrderID: pending.OrderID, EventID: unknownID, Outcome: "unknown"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(map[string]any{"as_of": time.Now().UTC(), "snapshot_id": snapshotID})
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(map[string]any{
		"job_schema_version": "0.1", "job_id": jobID, "portfolio": json.RawMessage(snapshot), "max_llm_calls": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := research.Create(ctx, jobs.CreateJob{ID: jobID, SnapshotID: snapshotID, Request: request, Snapshot: snapshot}, nil); err != nil {
		t.Fatal(err)
	}
	lease, err := research.Claim(ctx, "diagnostic-test", "00000000-0000-4000-8000-000000000012", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := research.AuthorizeCall(ctx, jobID, lease.WorkerID, lease.Token, "00000000-0000-4000-8000-000000000013"); err != nil {
		t.Fatal(err)
	}
	result := jobs.CompleteJob{
		JobID: jobID, WorkerID: lease.WorkerID, Token: lease.Token, ExpiresAt: lease.Deadline,
		ExpectedAttempt: 1, ExpectedLLMCalls: 1,
		Payload: json.RawMessage(`{"result_schema_version":"0.2","metadata":{"llm_calls":1,"missing_usage_calls":0}}`),
	}
	if _, _, err := research.Complete(ctx, result, func(jobs.Job, time.Time) error { return nil }); err != nil {
		t.Fatal(err)
	}
	readonly, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parameters := readonly.Query()
	parameters.Set("default_transaction_read_only", "on")
	readonly.RawQuery = parameters.Encode()
	args := []string{"--account", accountID, "--job", jobID}
	var stdout bytes.Buffer
	if code := run(ctx, readonly.String(), args, &stdout); code != 0 {
		t.Fatalf("healthy check exit %d: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"status":"ok"`) || !strings.Contains(stdout.String(), `"account_version":5`) ||
		!strings.Contains(stdout.String(), `"guaranteed_remaining_llm_calls":1`) {
		t.Fatalf("wrong healthy diagnostics: %s", stdout.String())
	}
	for _, injection := range []struct {
		query string
		args  []any
	}{
		{`UPDATE paper_accounts SET cash_usd=cash_usd+0.00000001 WHERE account_id=$1`, []any{accountID}},
		{`UPDATE paper_positions SET quantity_raw=quantity_raw+1 WHERE account_id=$1`, []any{accountID}},
		{`UPDATE paper_events SET recorded_at=clock_timestamp()-interval '20 minutes' WHERE event_id=$1`, []any{unknownID}},
		{`UPDATE paper_orders SET request=jsonb_set(request,'{expires_at}',to_jsonb($2::text)) WHERE order_id=$1`,
			[]any{pending.OrderID, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)}},
		{`UPDATE research_job_results SET payload=$2,payload_sha256=sha256($2) WHERE job_id=$1`,
			[]any{jobID, []byte(`{"result_schema_version":"0.2","metadata":{"llm_calls":2,"missing_usage_calls":1}}`)}},
		{`UPDATE research_jobs SET legacy_budget_unknown=true WHERE id=$1`, []any{jobID}},
		{`UPDATE research_call_permits SET remaining_llm_calls=0 WHERE job_id=$1`, []any{jobID}},
	} {
		if _, err := inspection.Exec(ctx, injection.query, injection.args...); err != nil {
			t.Fatal(err)
		}
	}
	stdout.Reset()
	if code := run(ctx, readonly.String(), args, &stdout); code != 1 {
		t.Fatalf("anomaly check exit %d: %s", code, stdout.String())
	}
	for _, code := range []string{"ledger_cash_mismatch", "ledger_position_mismatch", "paper_order_unknown_stuck", "paper_order_overdue",
		"research_usage_mismatch", "research_budget_legacy_unknown", "research_token_usage_missing", "research_permit_sequence_mismatch"} {
		if !strings.Contains(stdout.String(), `"code":"`+code+`"`) {
			t.Fatalf("missing %s: %s", code, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "postgresql://") || strings.Contains(stdout.String(), lease.Token) {
		t.Fatal("diagnostics exposed connection or lease credentials")
	}
	if strings.Contains(stdout.String(), `"guaranteed_remaining_llm_calls"`) {
		t.Fatal("inconsistent permit history reported a guaranteed balance")
	}
}

func TestPaperCheckRedactsInvalidConnection(t *testing.T) {
	var stdout bytes.Buffer
	args := []string{"--account", "00000000-0000-4000-8000-000000000001"}
	if code := run(context.Background(), "postgresql://owner:secretfixture@%invalid", args, &stdout); code != 2 {
		t.Fatalf("invalid DSN exit = %d", code)
	}
	if strings.Contains(stdout.String(), "secretfixture") || !strings.Contains(stdout.String(), `"status":"unavailable"`) {
		t.Fatalf("invalid DSN leaked or wrong status: %s", stdout.String())
	}
}

func diagnosticDatabase(t *testing.T) (context.Context, string, *pgx.Conn) {
	t.Helper()
	baseDSN := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	base, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "paper_check_" + strings.ToLower(rand.Text())
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
	parsed, err := url.Parse(baseDSN)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("test database must be a PostgreSQL URI")
	}
	params := parsed.Query()
	params.Set("search_path", schema)
	parsed.RawQuery = params.Encode()
	inspection, err := pgx.Connect(ctx, parsed.String())
	if err != nil {
		t.Fatal("connect to test schema failed")
	}
	t.Cleanup(func() { _ = inspection.Close(context.Background()) })
	return ctx, parsed.String(), inspection
}
