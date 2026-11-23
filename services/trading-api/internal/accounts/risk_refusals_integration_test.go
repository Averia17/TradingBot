package accounts

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRiskRefusalsDurableIdentityAndBoundedRead(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	createFixture(t, ctx, store, "1000")
	var now time.Time
	if err := store.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	request := RiskRefusalInput{
		AccountID: testAccountID, JobID: identifier(300), EvaluatedAt: now, ReserveBefore: now.Add(time.Minute),
		RiskRefusal: RiskRefusal{
			DecisionID: identifier(301), CandidateOrderID: identifier(302), SnapshotVersion: 1,
			RequestSHA256: strings.Repeat("a", 64), QuoteSHA256: strings.Repeat("b", 64), Reason: RiskMaximumFee,
		},
	}
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			if err := store.RecordRiskRefusal(ctx, request); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()

	conflict := request
	conflict.QuoteSHA256 = strings.Repeat("c", 64)
	if err := store.RecordRiskRefusal(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed payload reused refusal identity: %v", err)
	}
	expired := request
	expired.EvaluatedAt, expired.ReserveBefore = now.Add(-time.Minute), now.Add(-time.Second)
	if err := store.RecordRiskRefusal(ctx, expired); !errors.Is(err, ErrExpired) {
		t.Fatalf("recorded expired observation: %v", err)
	}
	var rows []RiskRefusal
	read := func(cutoff time.Time, expected int, truncated bool) {
		t.Helper()
		err := WithReadSnapshot(ctx, dsn, func(view ReadSnapshot) error {
			if cutoff.IsZero() {
				cutoff = view.Cutoff()
			}
			var limited bool
			var err error
			rows, limited, err = view.RiskRefusals(ctx, request.AccountID, request.JobID, cutoff)
			if err == nil && (len(rows) != expected || limited != truncated) {
				t.Fatalf("read returned %d rows, truncated %v", len(rows), limited)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	read(time.Time{}, 1, false)
	first := rows[0]
	store.Close()
	store = openLedger(t, ctx, dsn)
	read(first.FirstObservedAt, 1, false)
	if !reflect.DeepEqual(rows[0], first) {
		t.Fatalf("first durable observation changed after reopen: %+v vs %+v", first, rows[0])
	}
	read(first.FirstObservedAt.Add(-time.Microsecond), 0, false)

	for i := range MaxRiskRefusals {
		additional := request
		additional.CandidateOrderID = identifier(400 + i)
		if err := store.RecordRiskRefusal(ctx, additional); err != nil {
			t.Fatal(err)
		}
	}
	read(time.Time{}, MaxRiskRefusals, true)
	if _, _, err := store.Reserve(ctx, reserveFixture(1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRiskRefusal(ctx, request); !errors.Is(err, ErrStale) {
		t.Fatalf("stale account accepted a risk observation: %v", err)
	}
}
