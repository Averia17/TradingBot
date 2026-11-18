package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPaperDispatchRegistrationRollsBackAndNeverArmsOldJobs(t *testing.T) {
	ctx, dsn, inspection := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	input := testJob()
	input.PaperDispatch = &PaperDispatchInput{AccountID: "not-an-account", Request: json.RawMessage(`{}`)}
	if _, created, err := store.Create(ctx, input, nil); err == nil || created {
		t.Fatal("invalid paper dispatch was queued")
	}
	assertCounts(t, ctx, inspection, 0)
	input.PaperDispatch = nil
	if _, created, err := store.Create(ctx, input, nil); err != nil || !created {
		t.Fatalf("create plain research job: %t, %v", created, err)
	}
	input.PaperDispatch = &PaperDispatchInput{AccountID: "00000000-0000-4000-8000-000000000123", Request: json.RawMessage(`{}`)}
	if _, created, err := store.Create(ctx, input, nil); err != nil || created {
		t.Fatalf("replay plain research job: %t, %v", created, err)
	}
	var count int
	if err := inspection.QueryRow(ctx, "SELECT count(*) FROM research_paper_dispatch").Scan(&count); err != nil || count != 0 {
		t.Fatalf("old job opted into automatic application: %d, %v", count, err)
	}
	input.ID = "00000000-0000-4000-8000-000000000124"
	if _, created, err := store.Create(ctx, input, nil); err != nil || !created {
		t.Fatalf("create authorized paper review: %t, %v", created, err)
	}
	task, err := store.GetPaperDispatch(ctx, input.ID)
	if err != nil || task.JobID != input.ID || task.AccountID != input.PaperDispatch.AccountID {
		t.Fatalf("read queued dispatch binding: %+v, %v", task, err)
	}
	if err := store.FinishPaperDispatch(ctx, task, "hold"); !errors.Is(err, ErrConflict) {
		t.Fatalf("acknowledged a queued research job: %v", err)
	}
	input.PaperDispatch.Request = json.RawMessage(`{"changed":true}`)
	if _, created, err := store.Create(ctx, input, nil); !errors.Is(err, ErrConflict) || created {
		t.Fatalf("changed paper dispatch reused a job ID: %t, %v", created, err)
	}
}

func TestOwnerBoundPaperEntryCannotArmExistingResearch(t *testing.T) {
	ctx, dsn, db := testDatabase(t)
	store := openTestStore(t, ctx, dsn)
	ownerPin := strings.Repeat("a", 64)
	now := databaseNow(t, ctx, db)
	job := func(index int) CreateJob {
		input := queueInput(t, index, now.Add(-time.Minute))
		var wire map[string]any
		if err := json.Unmarshal(input.Request, &wire); err != nil {
			t.Fatal(err)
		}
		wire["job_schema_version"] = "0.2"
		wire["identity_registry"] = map[string]string{"ref": "identity.json", "sha256": ownerPin}
		input.Request, _ = json.Marshal(wire)
		return input
	}
	dispatch := &PaperDispatchInput{AccountID: testIdentity(500), Request: json.RawMessage(`{"enrolled":true}`)}
	plain := job(1)
	if _, created, err := store.Create(ctx, plain, nil); err != nil || !created {
		t.Fatalf("could not queue the plain research job: %t %v", created, err)
	}
	plain.PaperDispatch = dispatch
	bound := BoundPaperEntry{Job: plain, OwnerRegistrySHA256: ownerPin}
	if _, created, err := store.CreateBoundPaperEntry(ctx, bound, nil); !errors.Is(err, ErrConflict) || created {
		t.Fatalf("existing research became dispatchable: %t %v", created, err)
	}
	if _, err := store.GetPaperDispatch(ctx, plain.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("existing research gained a dispatch row: %v", err)
	}
	bound.Job = job(2)
	bound.Job.PaperDispatch = dispatch
	bound.OwnerRegistrySHA256 = strings.Repeat("b", 64)
	if _, created, err := store.CreateBoundPaperEntry(ctx, bound, nil); !errors.Is(err, ErrConflict) || created {
		t.Fatalf("wrong owner registry accepted: %t %v", created, err)
	}
	bound.OwnerRegistrySHA256 = ownerPin
	refused := errors.New("owner admission check denied")
	if _, created, err := store.CreateBoundPaperEntry(ctx, bound, func() error { return refused }); !errors.Is(err, refused) || created {
		t.Fatalf("failed owner admission was not rolled back: %t %v", created, err)
	}
	if _, err := store.Get(ctx, bound.Job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed admission left a research row: %v", err)
	}
	queued, created, err := store.CreateBoundPaperEntry(ctx, bound, nil)
	if err != nil || !created || queued.ID != bound.Job.ID {
		t.Fatalf("authorized owner did not create bound job: %+v %t %v", queued, created, err)
	}
	task, err := store.GetPaperDispatch(ctx, queued.ID)
	if err != nil || task.AccountID != dispatch.AccountID || !bytes.Equal(task.Request, dispatch.Request) {
		t.Fatalf("bound job lacks its exact durable dispatch: %+v %v", task, err)
	}
	replayed, created, err := store.CreateBoundPaperEntry(ctx, bound, func() error { return refused })
	if err != nil || created || !replayed.CreatedAt.Equal(queued.CreatedAt) {
		t.Fatalf("bound replay did not return original admission: %+v %t %v", replayed, created, err)
	}
}
