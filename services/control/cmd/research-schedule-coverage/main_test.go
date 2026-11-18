package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
)

func TestClassifyScheduleEvidence(t *testing.T) {
	at := time.Date(2026, 11, 18, 23, 59, 0, 0, time.UTC)
	plan := contract.ResearchCalendarOccurrence{ScheduledAt: at, ScheduleID: "fixture", MaxLatenessSeconds: 60, RequestSHA256: "expected"}
	observed := &jobs.ScheduleObservation{
		ScheduleID: plan.ScheduleID, ScheduledAt: at.Format(time.RFC3339Nano),
		MaxLatenessSeconds: plan.MaxLatenessSeconds, RequestSHA256: "expected", RecordedAt: plan.Deadline(),
	}
	matchingJob := func(createdAt time.Time) *jobs.ScheduleJobEvidence {
		return &jobs.ScheduleJobEvidence{CreatedAt: createdAt, RequestSHA256: "expected", RequestIntegrity: true, ResultIntegrity: true}
	}
	for _, tc := range []struct {
		name string
		now  time.Time
		row  jobs.ScheduleEvidence
		want string
	}{
		{"future", at.Add(-time.Second), jobs.ScheduleEvidence{}, "not_due"},
		{"accepting", at, jobs.ScheduleEvidence{}, "window_open"},
		{"closed without observation", plan.Deadline(), jobs.ScheduleEvidence{}, "missing_evidence"},
		{"closed with observation", plan.Deadline(), jobs.ScheduleEvidence{Observation: observed}, "observed_miss"},
		{"job accepted before deadline", plan.Deadline(), jobs.ScheduleEvidence{Job: matchingJob(at)}, "on_time"},
		{"job reconciled after deadline", plan.Deadline(), jobs.ScheduleEvidence{Observation: observed, Job: matchingJob(plan.Deadline())}, "late"},
		{"early job", plan.Deadline(), jobs.ScheduleEvidence{Job: matchingJob(at.Add(-time.Second))}, "early"},
		{"bad request digest", plan.Deadline(), jobs.ScheduleEvidence{Job: &jobs.ScheduleJobEvidence{CreatedAt: at, RequestSHA256: "expected"}}, "conflict"},
		{"wrong schedule window", plan.Deadline(), jobs.ScheduleEvidence{Observation: &jobs.ScheduleObservation{ScheduleID: "different"}}, "conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOccurrence(plan, tc.now, tc.row); got.SubmissionStatus != tc.want {
				t.Fatalf("status=%s, want %s (%+v)", got.SubmissionStatus, tc.want, got)
			}
		})
	}
}

func validateCoverageFixture(t *testing.T, raw []byte) {
	t.Helper()
	const id = "https://tradingbot.local/research-schedule-coverage.schema.json"
	schemaFile, err := os.ReadFile("../../../../contracts/research-schedule-coverage.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schemaDoc, value any
	if err := json.Unmarshal(schemaFile, &schemaDoc); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(id, schemaDoc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedCoverageFixtureMatchesContract(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/valid-research-schedule-coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	validateCoverageFixture(t, raw)
}

func TestCoverageRequiresPinnedSchedule(t *testing.T) {
	args := []string{"--config", "../../../../contracts/valid-research-schedule.json"}
	if err := run(context.Background(), "postgresql://invalid", args, io.Discard); err == nil {
		t.Fatal("expected a missing schedule digest to fail before connecting")
	}
	if err := run(context.Background(), "postgresql://invalid", append(args, "--sha256", strings.Repeat("a", 64)), io.Discard); err == nil ||
		!strings.Contains(err.Error(), "disagrees with the pinned SHA-256") {
		t.Fatalf("wrong schedule digest should fail before connecting, got %v", err)
	}
}

func TestEventOnlyScheduleCannotMasqueradeAsCalendarCoverage(t *testing.T) {
	const path = "../../../../contracts/valid-research-event-schedule.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--config", path, "--sha256", fmt.Sprintf("%x", sha256.Sum256(raw))}
	if err := run(context.Background(), "postgresql://invalid", args, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "no declared calendar occurrences") {
		t.Fatalf("event-only schedules cannot have calendar coverage, got %v", err)
	}
}
