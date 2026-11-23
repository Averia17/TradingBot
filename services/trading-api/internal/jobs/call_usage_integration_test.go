package jobs

import (
	"bytes"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"tradingbot/trading-api/internal/contract"
)

func TestResearchCallUsageSurvivesLeaseReplacementWithoutRefund(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	job := queueJob(t, ctx, store, 1, 0)
	first, err := store.Claim(ctx, "first", testIdentity(100), testSnapshotWindow)
	if err != nil {
		t.Fatal(err)
	}
	known, interrupted, unissued := testIdentity(201), testIdentity(202), testIdentity(203)
	for _, id := range []string{known, interrupted} {
		if _, created, err := store.AuthorizeCall(ctx, job.ID, first.WorkerID, first.Token, id); err != nil || !created {
			t.Fatalf("grant %s = %v/%v", id, created, err)
		}
	}
	input := callObservation(first, known)
	results := make([]CallUsageReceipt, 8)
	var pending sync.WaitGroup
	for index := range results {
		pending.Go(func() {
			receipt, _, err := store.RecordCallUsage(ctx, input)
			results[index] = receipt
			if err != nil {
				t.Errorf("concurrent observation: %v", err)
			}
		})
	}
	pending.Wait()
	if results[0].RecordedAt.IsZero() {
		t.Fatal("missing first receipt")
	}
	for _, receipt := range results[1:] {
		if receipt != results[0] {
			t.Fatalf("nonidentical receipt: %v, %v", receipt, results[0])
		}
	}
	if _, _, err := store.RecordCallUsage(ctx, callObservation(first, unissued)); !errors.Is(err, ErrCallPermitNotFound) {
		t.Fatalf("unissued call: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE research_job_attempts
		SET expires_at=clock_timestamp()-interval '1 microsecond' WHERE lease_token=$1`, first.Token); err != nil {
		t.Fatal(err)
	}
	store.Close()
	reopened := openTestStore(t, ctx, dsn)
	second, err := reopened.Claim(ctx, "second", testIdentity(101), testSnapshotWindow)
	if err != nil || second.Attempt != 2 {
		t.Fatalf("claim replacement: %+v/%v", second, err)
	}
	if replay, created, err := reopened.RecordCallUsage(ctx, input); err != nil || created || replay != results[0] {
		t.Fatalf("read committed after replacement: %+v/%v/%v", replay, created, err)
	}
	conflicting := input
	conflicting.Call.Status = "completed"
	if _, _, err := reopened.RecordCallUsage(ctx, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting outcome: %v", err)
	}
	if _, _, err := reopened.RecordCallUsage(ctx, callObservation(first, interrupted)); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("late new observation: %v", err)
	}
	if _, _, err := reopened.RecordCallUsage(ctx, callObservation(second, interrupted)); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("replacement cannot report a prior permit: %v", err)
	}
	if grant, _, err := reopened.AuthorizeCall(ctx, job.ID, second.WorkerID, second.Token, unissued); err != nil || grant.Remaining != 9 {
		t.Fatalf("remaining across failed attempt and write: %+v/%v", grant, err)
	}
	dbURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	readonly := dbURL.Query()
	readonly.Set("default_transaction_read_only", "on")
	dbURL.RawQuery = readonly.Encode()
	snapshot, err := ReadResearchCallUsage(ctx, dbURL.String(), job.ID)
	if err != nil || snapshot.Status != "leased" || snapshot.Attempts != 2 || len(snapshot.Records) != 3 ||
		snapshot.JobID != job.ID || snapshot.LegacyBudgetUnknown || !snapshot.PermitHistoryKnown {
		t.Fatalf("read-only snapshot = %+v/%v", snapshot, err)
	}
	row, unknown := snapshot.Records[0], snapshot.Records[1:]
	if row.CallID != known || row.Attempt != 1 || row.RecordedAt == nil ||
		row.GrantedAt.After(*row.RecordedAt) || row.PayloadSHA256 == "" ||
		!bytes.Contains(row.Payload, []byte(`"status":"failed"`)) ||
		bytes.Contains(row.Payload, []byte(first.Token)) {
		t.Fatalf("metered call = %+v", row)
	}
	for _, item := range unknown {
		if item.RecordedAt != nil || item.Payload != nil || item.PayloadSHA256 != "" {
			t.Fatalf("unmetered permit appears to have usage: %+v", item)
		}
	}
	if _, err := db.Exec(ctx, `DELETE FROM research_schema_migrations WHERE version=4`); err != nil {
		t.Fatal(err)
	}
	withoutMigration, err := ReadResearchCallUsage(ctx, dbURL.String(), job.ID)
	if err != nil || withoutMigration.PermitHistoryKnown || len(withoutMigration.Records) != 3 {
		t.Fatalf("missing migration metadata = %+v/%v", withoutMigration, err)
	}
	if _, err := db.Exec(ctx, `UPDATE research_call_usage SET payload_sha256=decode(repeat('00',32),'hex') WHERE job_id=$1 AND call_id=$2`, job.ID, known); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResearchCallUsage(ctx, dbURL.String(), job.ID); err == nil {
		t.Fatal("stored callback with mismatched hash was exported")
	}
}

func callObservation(lease Lease, callID string) RecordResearchCallUsage {
	provider, model := "offline", "scripted-offline-v1"
	return RecordResearchCallUsage{
		JobID: lease.Job.ID, WorkerID: lease.WorkerID, Token: lease.Token,
		Call: contract.ResearchLLMCall{RunID: callID, Provider: &provider, Model: &model,
			Role: "research", Status: "failed", StartedAt: time.Now().UTC(), UsageSource: "unavailable"},
	}
}
