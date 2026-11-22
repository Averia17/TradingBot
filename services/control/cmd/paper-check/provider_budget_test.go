package main

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"
	"time"

	"tradingbot/control/internal/jobs"
)

func TestBudgetReportsPreparedProviderUsage(t *testing.T) {
	load := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile("../../../../contracts/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	createdAt := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	job := jobs.Job{
		ID: "f700cba3-c634-4204-8669-1acd78612431", Status: "completed", CreatedAt: createdAt,
		Request: load("valid-research-provider-job.json"),
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(load("valid-prepared-provider-result-request.json"), &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "worker_id")
	delete(fields, "lease_token")
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	result := jobs.Result{JobID: job.ID, Attempt: 1, CompletedAt: createdAt.Add(time.Minute), Payload: body}
	limit := 1
	r := report{Research: &researchBudget{JobID: job.ID, Limit: &limit, UnconfirmedPermits: 1}}
	checkReportedUsage(&r, job, result, digest[:], 1, true)
	if len(r.Issues) != 0 || r.Research.UnconfirmedPermits != 0 ||
		r.Research.ReportedCalls == nil || *r.Research.ReportedCalls != 1 ||
		r.Research.MissingTokenCallUsage == nil || *r.Research.MissingTokenCallUsage != 0 {
		t.Fatalf("prepared result usage = %+v; issues = %+v", r.Research, r.Issues)
	}
	job.Request = load("valid-research-identity-job.json")
	checkReportedUsage(&r, job, result, digest[:], 1, true)
	if len(r.Issues) != 1 || r.Issues[0].Code != "research_result_integrity_failed" {
		t.Fatalf("cross-mode receipt issues = %+v", r.Issues)
	}
}
