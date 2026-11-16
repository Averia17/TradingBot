package papersim

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/control/internal/ledger"
)

func isolatedLedger(t *testing.T) (context.Context, *ledger.Store) {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("cannot connect to test database")
	}
	schema := "papersim_" + strings.ToLower(rand.Text())
	name := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer base.Close(cleanup)
		if _, err := base.Exec(cleanup, "DROP SCHEMA IF EXISTS "+name+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("test database must be a PostgreSQL URI")
	}
	params := parsed.Query()
	params.Set("search_path", schema)
	parsed.RawQuery = params.Encode()
	store, err := ledger.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal("cannot open paper ledger test schema")
	}
	t.Cleanup(store.Close)
	return ctx, store
}

type interruptedStore struct{ *ledger.Store }

var errInterrupted = errors.New("simulated interruption before writing the outcome")

func (interruptedStore) Resolve(context.Context, ledger.ResolutionRequest) (ledger.Order, bool, error) {
	return ledger.Order{}, false, errInterrupted
}

func TestRetryAfterReservationCommitsOnceInPostgres(t *testing.T) {
	ctx, store := isolatedLedger(t)
	now := time.Now().UTC()
	in, quote := fixture(now)
	raw := bindQuote(t, &in, quote)
	if _, _, err := store.CreateAccount(ctx, ledger.CreateAccount{ID: in.AccountID, InitialCashUSD: "1000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, interruptedStore{store}, in, raw, now); !errors.Is(err, errInterrupted) {
		t.Fatalf("expected the injected interruption: %v", err)
	}
	if account, err := store.GetAccount(ctx, in.AccountID); err != nil || account.ReservedCashUSD != "124.2" || account.CashUSD != "1000" {
		t.Fatalf("reservation did not survive: %+v, %v", account, err)
	}
	type result struct {
		value Result
		err   error
	}
	var calls sync.WaitGroup
	results := make(chan result, 2)
	for range 2 {
		calls.Go(func() {
			value, err := Run(ctx, store, in, raw, time.Now().UTC())
			results <- result{value, err}
		})
	}
	calls.Wait()
	close(results)
	inserted := 0
	for outcome := range results {
		if outcome.err != nil || outcome.value.Order.Status != "filled" || outcome.value.ReservationRecorded {
			t.Fatalf("retry: %+v, %v", outcome.value, outcome.err)
		}
		if outcome.value.OutcomeRecorded {
			inserted++
		}
	}
	if inserted != 1 {
		t.Fatalf("recorded fill %d times", inserted)
	}
	account, err := store.GetAccount(ctx, in.AccountID)
	if err != nil || account.CashUSD != "876.3480258" || account.ReservedCashUSD != "0" || len(account.Positions) != 1 || account.Positions[0].QuantityRaw != in.QuantityRaw {
		t.Fatalf("settlement: %+v, %v", account, err)
	}
	events, err := store.Events(ctx, in.AccountID, 0, 10)
	if err != nil || len(events) != 3 || events[2].Kind != "paper_order.filled" {
		t.Fatalf("events = %+v, %v", events, err)
	}
	// A new payload under that decision conflicts even if it uses the current version.
	changed := in
	changed.SnapshotVersion = account.Version
	changed.MaxFeeUSD = "0.30"
	if _, err := Run(ctx, store, changed, raw, time.Now().UTC()); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("different intent under same decision: %v", err)
	}
}

func TestRejectionReleasesAndTimeoutKeepsFundsInPostgres(t *testing.T) {
	ctx, store := isolatedLedger(t)
	now := time.Now().UTC()
	in, quote := fixture(now)
	if _, _, err := store.CreateAccount(ctx, ledger.CreateAccount{ID: in.AccountID, InitialCashUSD: "1000"}); err != nil {
		t.Fatal(err)
	}
	expired, stale := fixture(now.Add(-5 * time.Minute))
	oldQuote := bindQuote(t, &expired, stale)
	if _, err := Run(ctx, store, expired, oldQuote, now); !errors.Is(err, ledger.ErrExpired) {
		t.Fatalf("expired quote: %v", err)
	}
	if account, err := store.GetAccount(ctx, in.AccountID); err != nil || account.Version != 1 || account.ReservedCashUSD != "0" {
		t.Fatalf("expired quote left funds reserved: %+v, %v", account, err)
	}
	quote.Execution = "reject"
	raw := bindQuote(t, &in, quote)
	result, err := Run(ctx, store, in, raw, now)
	if err != nil || result.Order.Status != "cancelled" || result.Order.Fill != nil {
		t.Fatalf("fixture rejection: %+v, %v", result, err)
	}
	account, err := store.GetAccount(ctx, in.AccountID)
	if err != nil || account.ReservedCashUSD != "0" || account.CashUSD != "1000" {
		t.Fatalf("cancelled funds: %+v, %v", account, err)
	}
	in.SnapshotVersion = account.Version
	in.DecisionID = "33333333-3333-4333-8333-333333333333"
	quote.Execution = "timeout"
	raw = bindQuote(t, &in, quote)
	result, err = Run(ctx, store, in, raw, now)
	if err != nil || result.Order.Status != "unknown" || result.Order.Fill != nil {
		t.Fatalf("fixture timeout: %+v, %v", result, err)
	}
	account, err = store.GetAccount(ctx, in.AccountID)
	if err != nil || account.ReservedCashUSD != "124.2" || account.CashUSD != "1000" {
		t.Fatalf("unknown funds: %+v, %v", account, err)
	}
}
