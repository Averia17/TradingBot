package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/papernav"
)

const testAccount = "11111111-1111-4111-8111-111111111111"

func TestPaperNavReadsVersionedPostgresHistoryWithoutMigrations(t *testing.T) {
	ctx, conn, dsn := reportingDatabase(t)
	instrument := ledger.Instrument{ID: "stock:ABC", RegistryVersion: "fixture-v1", Decimals: 2}
	order := ledger.ReservationRequest{
		AccountID: testAccount, OrderID: "22222222-2222-4222-8222-222222222222",
		DecisionID: "88888888-8888-4888-8888-888888888888", SnapshotVersion: 1, Instrument: instrument, Side: "BUY", QuantityRaw: "200",
	}
	filled := ledger.ResolutionRequest{
		AccountID: testAccount, OrderID: order.OrderID, EventID: "33333333-3333-4333-8333-333333333333",
		Outcome: "filled", GrossUSD: "50.00000001", FeeUSD: "0.02", ObservedAt: instant("2020-01-01T23:58:00Z"),
	}
	first, last := instant("2020-01-01T12:00:00Z"), instant("2020-01-02T00:00:00Z")
	initialHash := sha256.Sum256([]byte("isolated paper NAV account fixture"))
	if _, err := conn.Exec(ctx, `INSERT INTO paper_accounts (account_id, creation_hash, cash_usd, created_at, version)
		VALUES ($1,$2,949.97999999,$3,3)`, testAccount, initialHash[:], first); err != nil {
		t.Fatal(err)
	}
	addEvent(t, ctx, conn, testAccount, 1, "paper_account.created", first, ledger.CreateAccount{ID: testAccount, InitialCashUSD: "1000"})
	addEvent(t, ctx, conn, order.OrderID, 2, "paper_order.reserved", filled.ObservedAt, order)
	// The simulated fill happened before midnight but entered the ledger
	// exactly at midnight, so it belongs only to the next UTC valuation.
	addEvent(t, ctx, conn, filled.EventID, 3, "paper_order.filled", last, filled)

	req := papernav.Request{
		Version: "0.1", AccountID: testAccount, StartDay: "2020-01-01", EndDay: "2020-01-03",
		MarkMaxAgeSeconds: 3600, Source: papernav.Source{Kind: "synthetic_fixture", ID: "postgres-paper-fixture"},
		Marks: []papernav.Mark{
			{ID: "closing", Instrument: instrument, PriceUSD: "32.00000008", ObservedAt: instant("2020-01-02T18:30:00-05:00"), AvailableAt: instant("2020-01-02T23:40:00Z")},
			{ID: "missed", Instrument: instrument, PriceUSD: "100", ObservedAt: instant("2020-01-02T23:58:20Z"), AvailableAt: instant("2020-01-03T00:00:00Z")},
		},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	input := filepath.Join(t.TempDir(), "nav-config.json")
	if err := os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--input", input, "--sha256", hex.EncodeToString(digest[:])}
	var firstRun bytes.Buffer
	if err := run(ctx, dsn, args, &firstRun); !errors.Is(err, errIncomplete) {
		t.Fatalf("read-only NAV run: %v", err)
	}
	var report papernav.Report
	if err := json.Unmarshal(firstRun.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.DailyNAV) != 3 || report.Funding.CashUSD != "1000" || !report.Funding.RecordedAt.Equal(first) {
		t.Fatalf("unexpected NAV report: %+v", report)
	}
	before, settled, stale := report.DailyNAV[0], report.DailyNAV[1], report.DailyNAV[2]
	if before.AccountVersion != 2 || before.NAVUSD == nil || *before.NAVUSD != "1000.00000000" ||
		len(before.Positions) != 0 || settled.AccountVersion != 3 || settled.NAVUSD == nil ||
		*settled.NAVUSD != "1013.98000015" || settled.CashUSD != "949.97999999" || settled.FeesUSD != "0.02" ||
		settled.Positions[0].Mark.ID != "closing" || settled.Positions[0].QuantityRaw != "200" ||
		stale.NAVUSD != nil || stale.Positions[0].Issue != "mark_stale" {
		t.Fatalf("incorrect recorded-time valuation: %+v", report.DailyNAV)
	}
	if len(settled.LedgerSHA256) != 64 || settled.LedgerSHA256 != stale.LedgerSHA256 || before.LedgerSHA256 == settled.LedgerSHA256 {
		t.Fatal("expected a stable, versioned ledger hash")
	}

	laterID := "44444444-4444-4444-8444-444444444444"
	later := ledger.ReservationRequest{
		AccountID: testAccount, OrderID: laterID, DecisionID: order.DecisionID, SnapshotVersion: 3, Instrument: instrument, Side: "BUY", QuantityRaw: "100",
	}
	err = ledger.WithReadSnapshot(ctx, dsn, func(view ledger.ReadSnapshot) error {
		addEvent(t, ctx, conn, laterID, 4, "paper_order.reserved", instant("2020-01-04T00:00:00Z"), later)
		events, err := view.Events(ctx, testAccount, 3, 100)
		if err != nil {
			return err
		}
		if len(events) != 0 {
			t.Fatal("read snapshot observed an event committed after the snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var replay bytes.Buffer
	if err := run(ctx, dsn, args, &replay); !errors.Is(err, errIncomplete) || !bytes.Equal(firstRun.Bytes(), replay.Bytes()) {
		t.Fatalf("later history changed the report for closed cutoffs: %v", err)
	}
}

// Each test gets an isolated schema and a separately enforced read-only DSN.
func reportingDatabase(t *testing.T) (context.Context, *pgx.Conn, string) {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("test database must be a PostgreSQL URI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	base, err := pgx.Connect(ctx, parsed.String())
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "paper_nav_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer base.Close(cleanup)
		if _, err := base.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	store, err := ledger.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	conn, err := pgx.Connect(ctx, parsed.String())
	if err != nil {
		t.Fatal("connect writer fixture failed")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	})
	q.Set("default_transaction_read_only", "on")
	parsed.RawQuery = q.Encode()
	return ctx, conn, parsed.String()
}

func addEvent(t *testing.T, ctx context.Context, conn *pgx.Conn, id string, version int, kind string, recordedAt time.Time, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	if _, err := conn.Exec(ctx, `INSERT INTO paper_events (account_id, event_id, account_version, kind, request_hash, payload, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, testAccount, id, version, kind, hash[:], raw, recordedAt); err != nil {
		t.Fatal(err)
	}
}

func instant(value string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return at
}
