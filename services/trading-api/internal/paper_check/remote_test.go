package paper_check

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/jobs"
)

func TestPaperCheckJoinsOwnerDiagnosticsOverHTTP(t *testing.T) {
	ctx, accountDSN, _ := diagnosticDatabase(t)
	_, jobsDSN, _ := diagnosticDatabase(t)
	paper, err := accounts.Open(ctx, accountDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paper.Close)
	research, err := jobs.Open(ctx, jobsDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(research.Close)

	const accountID = "00000000-0000-4000-8000-000000000021"
	const token = "fixture-token-1234567890-paper-internal"
	if _, _, err := paper.CreateAccount(ctx, accounts.CreateAccount{ID: accountID, InitialCashUSD: "10.00000001"}); err != nil {
		t.Fatal(err)
	}
	job := createDiagnosticJob(t, ctx, research, accountID, 22)
	jobID := job.ID

	accountsAPI := httptest.NewServer(NewAccountsHandler(accountDSN, token))
	t.Cleanup(accountsAPI.Close)
	jobsAPI := httptest.NewServer(NewJobsHandler(jobsDSN, token))
	t.Cleanup(jobsAPI.Close)
	t.Setenv("TRADINGBOT_OFFLINE_TEST_DATABASE_URL", "")
	t.Setenv("TRADINGBOT_PAPER_INTERNAL_TOKEN_FILE", "")
	t.Setenv("TRADINGBOT_ACCOUNTS_URL", accountsAPI.URL)
	t.Setenv("TRADINGBOT_TRADING_API_URL", jobsAPI.URL)
	t.Setenv("TRADINGBOT_PAPER_INTERNAL_TOKEN", token)

	args := []string{"--account", accountID, "--job", jobID, "--pipeline"}
	var stdout bytes.Buffer
	if status := Run(ctx, args, &stdout); status != 0 {
		t.Fatalf("queued diagnostic status=%d: %s", status, stdout.String())
	}
	var queued report
	if err := json.Unmarshal(stdout.Bytes(), &queued); err != nil || queued.Pipeline == nil ||
		len(queued.Pipeline.Jobs) != 1 || queued.Pipeline.Jobs[0].Stage != "queued" ||
		queued.AccountVersion != 1 || queued.Research == nil || queued.Research.JobID != jobID {
		t.Fatalf("unexpected queued diagnostics: %s (%v)", stdout.String(), err)
	}

	completeDiagnosticJob(t, ctx, research, jobID, 0)
	_, err = paper.RecordApplication(ctx, accounts.ApplicationInput{
		JobID: jobID, AccountID: accountID, SnapshotVersion: 1, RequestSHA256: sha256.Sum256(job.PaperDispatch.Request),
	}, "", []byte(`{"status":"hold"}`), time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if status := Run(ctx, args, &stdout); status != 0 {
		t.Fatalf("completed diagnostic status=%d: %s", status, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"stage":"awaiting_ack"`) || !strings.Contains(stdout.String(), `"application_receipt":true`) {
		t.Fatalf("account application was not joined to research dispatch: %s", stdout.String())
	}

	jobsAPI.Close()
	stdout.Reset()
	if status := Run(ctx, []string{"--account", accountID}, &stdout); status != 0 {
		t.Fatalf("jobs outage affected accounts-only diagnostics: %d: %s", status, stdout.String())
	}
	stdout.Reset()
	if status := Run(ctx, args, &stdout); status != 2 || !strings.Contains(stdout.String(), `"status":"unavailable"`) {
		t.Fatalf("jobs outage reported a healthy pipeline: %d: %s", status, stdout.String())
	}
}
