package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestReadCompletedPaperResearchVerifiesExactDurableDispatch(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	readOnly, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	options := readOnly.Query()
	options.Set("default_transaction_read_only", "on")
	readOnly.RawQuery = options.Encode()
	registry := strings.Repeat("7", 64)
	newJob := func(index int, bound bool) CreateJob {
		t.Helper()
		input := queueInput(t, index, databaseNow(t, ctx, db))
		if !bound {
			return input
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(input.Request, &wire); err != nil {
			t.Fatal(err)
		}
		wire["job_schema_version"] = json.RawMessage(`"0.2"`)
		wire["identity_registry"], _ = json.Marshal(map[string]string{"ref": "registry.json", "sha256": registry})
		input.Request, _ = json.Marshal(wire)
		return input
	}
	complete := func(input CreateJob, registrySHA string, claim int) CompleteJob {
		t.Helper()
		var lease Lease
		var err error
		if registrySHA == "" {
			lease, err = store.Claim(ctx, "readonly-report-fixture", testIdentity(claim), testSnapshotWindow)
		} else {
			lease, err = store.ClaimWithIdentityRegistry(ctx, "readonly-report-fixture", testIdentity(claim), testSnapshotWindow, registrySHA)
		}
		if err != nil || lease.Job.ID != input.ID {
			t.Fatalf("claim read-only fixture: %+v %v", lease, err)
		}
		result := attemptResult(lease, "read-only fixture")
		if _, created, err := store.Complete(ctx, result, func(Job, time.Time) error { return nil }); err != nil || !created {
			t.Fatalf("complete read-only fixture: %t %v", created, err)
		}
		return result
	}
	legacy := newJob(1, false)
	if _, created, err := store.Create(ctx, legacy, nil); err != nil || !created {
		t.Fatalf("create legacy fixture: %t %v", created, err)
	}
	complete(legacy, "", 100)
	plain, err := ReadCompletedPaperResearch(ctx, readOnly.String(), legacy.ID)
	if err != nil || plain.Job.ID != legacy.ID || plain.Result.JobID != legacy.ID || plain.Dispatch != nil {
		t.Fatalf("legacy receipt should not need dispatch: %+v %v", plain, err)
	}

	bound := newJob(2, true)
	bound.PaperDispatch = &PaperDispatchInput{AccountID: testIdentity(900), Request: json.RawMessage(`{"approved":true}`)}
	if _, created, err := store.CreateBoundPaperEntry(ctx, BoundPaperEntry{Job: bound, OwnerRegistrySHA256: registry}, nil); err != nil || !created {
		t.Fatalf("create bound fixture: %t %v", created, err)
	}
	result := complete(bound, registry, 101)
	approved, err := ReadCompletedPaperResearch(ctx, readOnly.String(), bound.ID)
	if err != nil || approved.Dispatch == nil || approved.Dispatch.AccountID != bound.PaperDispatch.AccountID ||
		approved.Dispatch.JobID != bound.ID || !bytes.Equal(approved.Dispatch.Request, bound.PaperDispatch.Request) ||
		!bytes.Equal(approved.Job.Request, bound.Request) || !bytes.Equal(approved.Result.Payload, result.Payload) {
		t.Fatalf("approved completion did not carry exact durable proof: %+v %v", approved, err)
	}
	if _, err := db.Exec(ctx, `UPDATE research_paper_dispatch SET request=$2 WHERE job_id=$1`, bound.ID, []byte(`{"approved":false}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCompletedPaperResearch(ctx, readOnly.String(), bound.ID); err == nil || !strings.Contains(err.Error(), "does not match its hash") {
		t.Fatalf("edited dispatch supplied proof: %v", err)
	}

	researchOnly := newJob(3, true)
	if _, created, err := store.Create(ctx, researchOnly, nil); err != nil || !created {
		t.Fatalf("create plain bound research: %t %v", created, err)
	}
	complete(researchOnly, registry, 102)
	if _, err := ReadCompletedPaperResearch(ctx, readOnly.String(), researchOnly.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("plain research could supply owner proof: %v", err)
	}
}
