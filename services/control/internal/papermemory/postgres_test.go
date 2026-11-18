package papermemory

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"tradingbot/control/internal/ledger"
)

func TestOutcomeProjectionMatchesPostgresLedger(t *testing.T) {
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("test database must be a PostgreSQL URI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "paper_outcomes_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer base.Close(cleanup)
		if _, err := base.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	params := parsed.Query()
	params.Set("search_path", schema)
	parsed.RawQuery = params.Encode()
	store, err := ledger.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, created, err := store.CreateAccount(ctx, ledger.CreateAccount{ID: testAccount, InitialCashUSD: "1000"}); err != nil || !created {
		t.Fatalf("create paper account: %v", err)
	}

	settle := func(orderID int, version int64, side, units, gross, fee string) {
		t.Helper()
		order := ledger.ReservationRequest{
			AccountID: testAccount, OrderID: eventID(orderID), DecisionID: eventID(orderID + 100), QuoteID: eventID(orderID + 200),
			SnapshotVersion: version, Instrument: testInstrument, Side: side, QuantityRaw: units,
			GrossUSD: gross, MaxFeeUSD: fee, ExpiresAt: time.Now().UTC().Add(time.Minute),
		}
		accepted, created, err := store.Reserve(ctx, order)
		if err != nil || !created {
			t.Fatalf("reserve paper order: %v", err)
		}
		resolution := ledger.ResolutionRequest{
			AccountID: testAccount, OrderID: accepted.OrderID, EventID: eventID(orderID + 1), Outcome: "filled",
			GrossUSD: gross, FeeUSD: fee, ObservedAt: accepted.CreatedAt,
		}
		if _, created, err := store.Resolve(ctx, resolution); err != nil || !created {
			t.Fatalf("settle paper fill: %v", err)
		}
	}
	settle(2, 1, "BUY", "3", "10.00000001", "0.00000002")
	settle(4, 3, "SELL", "2", "12", "0.00000001")
	events, err := store.Events(ctx, testAccount, 0, 100)
	if err != nil || len(events) != 5 {
		t.Fatalf("paper event history: len %d, err %v", len(events), err)
	}
	beforeSale, err := Load(ctx, store, testAccount, events[2].RecordedAt)
	if err != nil || len(beforeSale.Realized) != 0 || len(beforeSale.OpenLots) != 1 || beforeSale.OpenLots[0].QuantityRaw != "3" {
		t.Fatalf("before sale = %+v, %v", beforeSale, err)
	}
	view, err := Load(ctx, store, testAccount, events[4].RecordedAt)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.GetAccount(ctx, testAccount)
	if err != nil || account.Version != view.AccountVersion || account.CashUSD != view.CashUSD || len(account.Positions) != 1 || len(view.OpenLots) != 1 || account.Positions[0].QuantityRaw != view.OpenLots[0].QuantityRaw {
		t.Fatalf("paper ledger and outcome view diverged: account %+v, view %+v, %v", account, view, err)
	}
	if view.RealizedNetPnLUSD != "5.33333331" || view.RecordedFeesUSD != "0.00000003" || view.OpenLots[0].CostBasisUSD != "3.33333335" {
		t.Fatalf("unexpected amounts: %+v", view)
	}

	order, created, err := store.Reserve(ctx, ledger.ReservationRequest{
		AccountID: testAccount, OrderID: eventID(6), DecisionID: eventID(106), QuoteID: eventID(206),
		SnapshotVersion: account.Version, Instrument: testInstrument, Side: "BUY", QuantityRaw: "4",
		GrossUSD: "16", MaxFeeUSD: "0.02", ExpiresAt: time.Now().UTC().Add(time.Minute),
	})
	if err != nil || !created {
		t.Fatalf("reserve partial paper order: %v", err)
	}
	for _, fill := range []ledger.ResolutionRequest{
		{AccountID: testAccount, OrderID: order.OrderID, EventID: eventID(7), Outcome: "partially_filled", QuantityRaw: "2", GrossUSD: "7", FeeUSD: "0.01", ObservedAt: order.CreatedAt},
		{AccountID: testAccount, OrderID: order.OrderID, EventID: eventID(8), Outcome: "filled", GrossUSD: "8", FeeUSD: "0.01", ObservedAt: order.CreatedAt},
	} {
		if _, created, err := store.Resolve(ctx, fill); err != nil || !created {
			t.Fatalf("settle paper quantity: %v", err)
		}
	}
	events, err = store.Events(ctx, testAccount, 5, 100)
	if err != nil || len(events) != 3 {
		t.Fatalf("partial paper event history: len %d, err %v", len(events), err)
	}
	partial, err := Load(ctx, store, testAccount, events[1].RecordedAt)
	if err != nil || len(partial.OpenLots) != 2 || partial.OpenLots[1].QuantityRaw != "2" || partial.OpenLots[1].CostBasisUSD != "7.01" {
		t.Fatalf("unexpected partial snapshot: %+v, %v", partial, err)
	}
	final, err := Load(ctx, store, testAccount, events[2].RecordedAt)
	if err != nil || len(final.OpenLots) != 3 || final.OpenLots[2].QuantityRaw != "2" || final.OpenLots[2].CostBasisUSD != "8.01" {
		t.Fatalf("unexpected final snapshot: %+v, %v", final, err)
	}
	account, err = store.GetAccount(ctx, testAccount)
	if err != nil || account.Version != final.AccountVersion || account.CashUSD != final.CashUSD || account.ReservedCashUSD != "0" ||
		len(account.Positions) != 1 || account.Positions[0].QuantityRaw != "5" || final.RecordedFeesUSD != "0.02000003" {
		t.Fatalf("final projection diverged from ledger: account %+v, view %+v, %v", account, final, err)
	}

	settle(20, account.Version, "SELL", "1", "1", "0")
	assertLessonJournal(t, ctx, parsed.String(), store, base, quoted)
}

func assertLessonJournal(t *testing.T, ctx context.Context, dsn string, store *ledger.Store, base *pgx.Conn, schema string) {
	t.Helper()
	account, err := store.GetAccount(ctx, testAccount)
	if err != nil {
		t.Fatal(err)
	}
	var first, second ledger.LessonFact
	err = ledger.WithReadSnapshot(ctx, dsn, func(snapshot ledger.ReadSnapshot) error {
		if _, err := BuildLesson(ctx, snapshot, testAccount, eventID(3), snapshot.Cutoff()); !errors.Is(err, ledger.ErrNotFound) {
			t.Fatalf("BUY fill available as a realized sale: %v", err)
		}
		var err error
		first, err = BuildLesson(ctx, snapshot, testAccount, eventID(5), snapshot.Cutoff())
		if err != nil {
			return err
		}
		second, err = BuildLesson(ctx, snapshot, testAccount, eventID(21), snapshot.Cutoff())
		return err
	})
	if err != nil || first.NetPnLUSD != "5.33333331" || first.SourceDecisionID != eventID(104) || first.SourceAccountVersion != 5 ||
		second.RealizedOutcome != "loss" || second.SourceOutcomeSHA256 == first.SourceOutcomeSHA256 {
		t.Fatalf("invalid derived lessons: %+v / %+v, %v", first, second, err)
	}
	for _, invalid := range []func(*ledger.LessonFact){
		func(f *ledger.LessonFact) { f.NetPnLUSD = "5.33333332" },
		func(f *ledger.LessonFact) { f.RealizedOutcome = "loss" },
		func(f *ledger.LessonFact) { f.ExitFeeErasedGain = true },
		func(f *ledger.LessonFact) { f.SourceAccountVersion-- },
		func(f *ledger.LessonFact) { f.SourceFillEventID = eventID(3) },
	} {
		tampered := first
		invalid(&tampered)
		if _, err := store.RecordLesson(ctx, tampered, eventID(50)); !errors.Is(err, ledger.ErrInvalid) {
			t.Fatalf("accepted invalid paper memory fact %+v: %v", tampered, err)
		}
	}
	initial, err := store.RecordLesson(ctx, first, eventID(50))
	if err != nil || initial.Revision != 1 || initial.Action != "record" || initial.AvailableAt.Before(first.SourceAvailableAt) {
		t.Fatalf("record initial lesson: %+v, %v", initial, err)
	}
	repeat, err := store.RecordLesson(ctx, first, eventID(50))
	if err != nil || repeat != initial {
		t.Fatalf("retry changed lesson: %+v, %v", repeat, err)
	}
	if _, err := store.RecordLesson(ctx, second, eventID(50)); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("reused operation ID did not conflict: %v", err)
	}
	if _, err := store.RecordLesson(ctx, first, eventID(54)); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("reused fill with a different operation did not conflict: %v", err)
	}
	other, err := store.RecordLesson(ctx, second, eventID(51))
	if err != nil || other.LessonID == initial.LessonID {
		t.Fatalf("second same-day lesson lost: %+v, %v", other, err)
	}
	if got, err := store.Lessons(ctx, testAccount, "", initial.AvailableAt.Add(-time.Microsecond), false, 100); err != nil || len(got) != 0 {
		t.Fatalf("future lesson visible: %+v, %v", got, err)
	}
	type correctionResult struct {
		event ledger.LessonEvent
		err   error
	}
	corrections := make(chan correctionResult, 2)
	for _, operation := range []string{eventID(52), eventID(53)} {
		go func() {
			event, err := store.CorrectLesson(ctx, testAccount, initial.LessonID, operation, "retract", 1)
			corrections <- correctionResult{event, err}
		}()
	}
	var retracted ledger.LessonEvent
	var conflicts int
	for range 2 {
		result := <-corrections
		if result.err == nil {
			retracted = result.event
		} else if errors.Is(result.err, ledger.ErrConflict) {
			conflicts++
		}
	}
	if conflicts != 1 || retracted.Revision != 2 || retracted.Fact != initial.Fact || !retracted.AvailableAt.After(initial.AvailableAt) {
		t.Fatalf("expected one immutable correction: %+v; conflicts %d", retracted, conflicts)
	}
	kept, err := store.Lessons(ctx, testAccount, initial.LessonID, retracted.AvailableAt.Add(-time.Microsecond), false, 100)
	if err != nil || len(kept) != 1 || kept[0] != initial {
		t.Fatalf("retraction changed the earlier cutoff: %+v, %v", kept, err)
	}
	readBack, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer readBack.Close()
	latest, err := readBack.Lessons(ctx, testAccount, initial.LessonID, time.Time{}, false, 1)
	if err != nil || len(latest) != 1 || latest[0] != retracted {
		t.Fatalf("latest state after reconnect: %+v, %v", latest, err)
	}
	if got, err := readBack.CorrectLesson(ctx, testAccount, initial.LessonID, retracted.EventID, "retract", 1); err != nil || got != retracted {
		t.Fatalf("retry retraction after reconnect: %+v, %v", got, err)
	}
	if _, err := readBack.CorrectLesson(ctx, testAccount, initial.LessonID, retracted.EventID, "reinstate", 2); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("changed correction under same operation ID: %v", err)
	}
	reinstated, err := readBack.CorrectLesson(ctx, testAccount, initial.LessonID, eventID(55), "reinstate", 2)
	if err != nil || reinstated.Revision != 3 || reinstated.Fact != initial.Fact {
		t.Fatalf("reinstate: %+v, %v", reinstated, err)
	}
	history, err := readBack.Lessons(ctx, testAccount, initial.LessonID, time.Time{}, true, 100)
	if err != nil || len(history) != 3 || history[0] != reinstated || history[1] != retracted || history[2] != initial {
		t.Fatalf("lesson audit history lost: %+v, %v", history, err)
	}
	if _, err := base.Exec(ctx, "UPDATE "+schema+".paper_memory_lessons SET fact=fact WHERE account_id=$1", testAccount); err == nil {
		t.Fatal("SQL allowed rewriting a lesson")
	}
	if _, err := base.Exec(ctx, "DELETE FROM "+schema+".paper_memory_lesson_events WHERE account_id=$1", testAccount); err == nil {
		t.Fatal("SQL allowed deleting its history")
	}
	after, err := store.GetAccount(ctx, testAccount)
	if err != nil || after.Version != account.Version || after.CashUSD != account.CashUSD {
		t.Fatalf("lesson changed the economic ledger: %+v, %v", after, err)
	}
}
