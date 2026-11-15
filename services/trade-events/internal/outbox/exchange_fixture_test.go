package outbox

import (
	"errors"
	"os"
	"testing"

	"tradingbot/trade-events/internal/fill_event"
)

func TestExchangeFixtureStore(t *testing.T) {
	ctx, dsn, db := testPostgres(t)
	outbox, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	if err := outbox.PrepareExchangeFixtures(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../../../contracts/valid-trade-event.json")
	if err != nil {
		t.Fatal(err)
	}
	event, err := fill_event.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	event.Source = fill_event.SourceExchangeFixture

	const retries = 6
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, retries)
	for range retries {
		go func() {
			created, err := outbox.RecordExchangeFixture(ctx, event)
			results <- result{created, err}
		}()
	}
	created := 0
	for range retries {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created %d fixture fills, want 1", created)
	}
	modification := event
	modification.Quantity = "2"
	if _, err := outbox.RecordExchangeFixture(ctx, modification); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay error = %v, want conflict", err)
	}

	// Seed a different outbox payload under the next ID so enqueue rejects it
	// after the fixture INSERT. The failed transaction must leave no fixture row.
	modification.ID = "paper:test:conflicting-outbox"
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := outbox.EnqueueTx(ctx, tx, modification); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	event.ID = modification.ID
	if _, err := outbox.RecordExchangeFixture(ctx, event); !errors.Is(err, ErrConflict) {
		t.Fatalf("outbox conflict error = %v", err)
	}
	var fixtures, tasks int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM exchange_fixture_fills),
		(SELECT count(*) FROM pgqueue_tasks)`).Scan(&fixtures, &tasks); err != nil {
		t.Fatal(err)
	}
	if fixtures != 1 || tasks != 2 {
		t.Fatalf("got %d fixtures and %d pgqueue tasks, want 1 fixture and 2 tasks", fixtures, tasks)
	}
}
