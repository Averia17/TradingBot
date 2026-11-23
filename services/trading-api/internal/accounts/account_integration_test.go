package accounts

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
)

const testAccountID = "e36eed0a-2a01-44ce-bd46-6f75c5352f7c"

func ledgerDB(t *testing.T) (context.Context, string) {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "paper_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer base.Close(cleanup)
		if _, err := base.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("test database must be a PostgreSQL URI")
	}
	params := parsed.Query()
	params.Set("search_path", schema)
	parsed.RawQuery = params.Encode()
	return ctx, parsed.String()
}

func openLedger(t *testing.T, ctx context.Context, dsn string) *Store {
	t.Helper()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestPaperAccountCreatesExactlyOnceAndPersistsAcrossRestart(t *testing.T) {
	ctx, dsn := ledgerDB(t)
	store := openLedger(t, ctx, dsn)
	initial := CreateAccount{ID: testAccountID, InitialCashUSD: "9007199254740993.12345678"}
	if _, err := store.GetAccount(ctx, initial.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	account, created, err := store.CreateAccount(ctx, initial)
	if err != nil || !created || account.CashUSD != initial.InitialCashUSD || account.ReservedCashUSD != "0" || account.Version != 1 || account.Positions == nil || len(account.Positions) != 0 {
		t.Fatalf("create account = %+v, %v, %v", account, created, err)
	}
	store.Close()
	reopened := openLedger(t, ctx, dsn)
	replay, created, err := reopened.CreateAccount(ctx, initial)
	if err != nil || created || replay.CashUSD != account.CashUSD || !replay.CreatedAt.Equal(account.CreatedAt) {
		t.Fatalf("replay after restart = %+v, %v, %v", replay, created, err)
	}
	if _, _, err := reopened.CreateAccount(ctx, CreateAccount{ID: initial.ID, InitialCashUSD: "1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting account creation: %v", err)
	}
	if _, _, err := reopened.CreateAccount(ctx, CreateAccount{ID: initial.ID, InitialCashUSD: "0.000000001"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsupported precision: %v", err)
	}
	account, err = reopened.GetAccount(ctx, initial.ID)
	if err != nil || account.CashUSD != initial.InitialCashUSD || account.Version != 1 {
		t.Fatalf("account changed after a conflict: %+v, %v", account, err)
	}
	var events int
	if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM paper_events WHERE account_id=$1`, initial.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("paper account events = %d, %v", events, err)
	}
}
