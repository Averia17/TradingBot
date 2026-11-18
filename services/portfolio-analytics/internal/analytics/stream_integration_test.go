package analytics

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestKafkaAndPostgresRecoverAccountWithReorderedEvents(t *testing.T) {
	store, dsn := testStore(t)
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, AccountTopic, HeadTopic, ValuationTopic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	producer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(producer.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	run := func(store *Store) (context.CancelFunc, <-chan error) {
		ctx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- Consume(ctx, store, cluster.ListenAddrs(), "analytics-integration", testAccount, nil) }()
		return stop, done
	}
	send := func(topic string, value any) {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := producer.ProduceSync(ctx, &kgo.Record{Topic: topic, Key: []byte(testAccount), Value: body}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	events := scenario()
	stop, done := run(store)
	send(AccountTopic, events[0])
	send(AccountTopic, events[6])
	send(HeadTopic, AccountHead{WireVersion, testAccount, 7, time.Now().UTC()})
	gap := waitReport(t, ctx, store, func(r Report) bool { return r.AccountStatus == "gap" })
	if gap.AccountVersion != 1 || gap.CashUSD != "1000" || gap.NAVUSD != nil {
		t.Fatalf("incomplete ledger appeared applied: %+v", gap)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	stop, done = run(store)
	defer stop()
	for _, event := range events[1:6] {
		send(AccountTopic, event)
	}
	send(AccountTopic, events[6])
	quote := mark("priced", "22", time.Now().UTC().Add(-time.Second))
	quote.AccountID, quote.Version = testAccount, WireVersion
	archive := filepath.Join(t.TempDir(), "marks.json")
	body, err := json.Marshal([]Valuation{quote})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, body, 0600); err != nil {
		t.Fatal(err)
	}
	if count, hash, err := PublishArchive(ctx, cluster.ListenAddrs(), testAccount, archive); err != nil || count != 1 || len(hash) != 64 {
		t.Fatalf("publish fixture archive: count %d, hash %s, err %v", count, hash, err)
	}
	complete := waitReport(t, ctx, store, func(r Report) bool { return r.Status == "complete" && r.AccountVersion == 7 })
	checkDecimal(t, complete.NAVUSD, "1013.5")
	checkDecimal(t, complete.RealizedPnLUSD, "5.25")
	checkDecimal(t, complete.UnrealizedPnLUSD, "8.25")

	router := HTTPHandler(store, testAccount, testPolicy(), nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/portfolio?period=7d", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("report HTTP %d: %s", response.Code, response.Body.String())
	}
	var report Report
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	checkDecimal(t, report.NAVUSD, "1013.5")
	if report.Performance.Period != "7d" || report.ReportID == "" {
		t.Fatalf("HTTP report: %+v", report)
	}

	conflict := events[6]
	conflict.ReservedCashUSD = "1"
	send(AccountTopic, conflict)
	select {
	case err := <-done:
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate must stop without confirming its offset: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("consumer did not report conflicting identity")
	}
}

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test PostgreSQL configuration")
	}
	connection, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("connect to test PostgreSQL failed")
	}
	schema := "portfolio_analytics_" + strings.ToLower(rand.Text())
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = connection.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		_ = connection.Close(ctx)
	})
	// Reuse the original DSN to keep its network and SSL properties intact.
	separator := "&"
	if !strings.Contains(dsn, "?") {
		separator = "?"
	}
	dsn += separator + "search_path=" + schema
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store, dsn
}

func waitReport(t *testing.T, ctx context.Context, store *Store, found func(Report) bool) Report {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		report, err := store.Report(ctx, testAccount, testPolicy(), "30d")
		if err == nil && found(report) {
			return report
		}
		if err != nil && !errors.Is(err, ErrNoAccount) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("expected projection was not visible, last report %+v: %v", report, ctx.Err())
		case <-ticker.C:
		}
	}
}
