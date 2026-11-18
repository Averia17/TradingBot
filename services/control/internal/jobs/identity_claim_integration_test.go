package jobs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClaimsSeparateRegistryBoundJobsFromLegacyWorkers(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	registry := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	input := queueInput(t, 1, databaseNow(t, ctx, db).Add(-time.Minute))
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(input.Request, &wire); err != nil {
		t.Fatal(err)
	}
	wire["job_schema_version"] = json.RawMessage(`"0.2"`)
	registryRef, _ := json.Marshal(map[string]string{"ref": "identity.json", "sha256": registry})
	wire["identity_registry"] = registryRef
	input.Request, _ = json.Marshal(wire)
	withPaperDispatch := input
	withPaperDispatch.PaperDispatch = &PaperDispatchInput{}
	if _, _, err := store.Create(ctx, withPaperDispatch, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("bound research job accepted a paper dispatch: %v", err)
	}
	if _, created, err := store.Create(ctx, input, nil); err != nil || !created {
		t.Fatalf("queue registry-bound job: created=%t error=%v", created, err)
	}
	legacy := queueJob(t, ctx, store, 2, -time.Minute)
	legacyLease, err := store.Claim(ctx, "legacy", testIdentity(100), testSnapshotWindow)
	if err != nil || legacyLease.Job.ID != legacy.ID {
		t.Fatalf("legacy worker did not skip the older bound job: %+v %v", legacyLease, err)
	}
	if _, err := store.ClaimWithIdentityRegistry(ctx, "legacy", testIdentity(100), testSnapshotWindow, registry); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("changed claim eligibility was accepted: %v", err)
	}
	nextLegacy := queueJob(t, ctx, store, 3, -time.Minute)
	if _, err := store.ClaimWithIdentityRegistry(ctx, "bound", testIdentity(101), testSnapshotWindow, other); !errors.Is(err, ErrNoJob) {
		t.Fatalf("wrong registry worker could lease the bound job: %v", err)
	}
	lease, err := store.ClaimWithIdentityRegistry(ctx, "bound", testIdentity(101), testSnapshotWindow, registry)
	if err != nil || lease.Job.ID != input.ID || lease.Attempt != 1 {
		t.Fatalf("bound worker did not get its job: %+v %v", lease, err)
	}
	replay, err := store.ClaimWithIdentityRegistry(ctx, "bound", testIdentity(101), testSnapshotWindow, registry)
	if err != nil || replay.Token != lease.Token {
		t.Fatalf("exact claim replay changed the attempt: %+v %v", replay, err)
	}
	if _, err := store.ClaimWithIdentityRegistry(ctx, "bound", testIdentity(104), testSnapshotWindow, registry); !errors.Is(err, ErrNoJob) {
		t.Fatalf("bound archive worker could take a legacy job: %v", err)
	}
	if next, err := store.Claim(ctx, "legacy", testIdentity(105), testSnapshotWindow); err != nil || next.Job.ID != nextLegacy.ID {
		t.Fatalf("legacy job was not preserved for a legacy worker: %+v %v", next, err)
	}
	forceAttemptTimes(t, ctx, db, lease.Token, -time.Second, time.Minute)
	if _, err := store.Claim(ctx, "legacy", testIdentity(102), testSnapshotWindow); !errors.Is(err, ErrNoJob) {
		t.Fatalf("legacy worker could retry an expired bound job: %v", err)
	}
	retry, err := store.ClaimWithIdentityRegistry(ctx, "bound", testIdentity(103), testSnapshotWindow, registry)
	if err != nil || retry.Job.ID != input.ID || retry.Attempt != 2 || retry.Token == lease.Token {
		t.Fatalf("bound retry lost its identity: %+v %v", retry, err)
	}
}
