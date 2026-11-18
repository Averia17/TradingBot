package jobs

import (
	"encoding/json"
	"errors"
	"testing"
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
