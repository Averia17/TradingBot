package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paperapply"
	"tradingbot/control/internal/paperrisk"
	"tradingbot/control/internal/papersim"
)

const (
	testJobID     = "22222222-2222-4222-8222-222222222222"
	testAccountID = "11111111-1111-4111-8111-111111111111"
	testDecision  = "33333333-3333-4333-8333-333333333333"
)

func TestPublishedTapeAndStrictPostCompletionWindow(t *testing.T) {
	contracts := filepath.Join("..", "..", "..", "..", "contracts")
	raw, err := os.ReadFile(filepath.Join(contracts, "valid-paper-quote-tape.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeTape(raw, strings.Repeat("a", 64), testJobID); err == nil {
		t.Fatal("expected digest mismatch")
	}
	tape, err := decodeTape(raw, sha256Hex(raw), testJobID)
	if err != nil {
		t.Fatal(err)
	}
	obs := tape.Observations[0]
	quote, err := papersim.ParseQuote(obs.Raw)
	if err != nil {
		t.Fatal(err)
	}
	before := quote.ObservedAt.Add(-time.Second)
	after := obs.Available.Add(time.Second)
	for _, test := range []struct {
		name            string
		completion, now time.Time
		deadline        time.Time
		want            bool
	}{
		{"eligible", before, after, quote.ExpiresAt, true},
		{"observed at receipt", quote.ObservedAt, after, quote.ExpiresAt, false},
		{"available at receipt", obs.Available, after, quote.ExpiresAt, false},
		{"available at DB time", before, obs.Available, quote.ExpiresAt, false},
		{"available at expiry", before, after, obs.Available, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, got, err := tape.eligible(obs, test.completion, test.now, test.deadline)
			if err != nil || got != test.want {
				t.Fatalf("eligible = %v, %v", got, err)
			}
		})
	}
}

func TestLocalTapeCLIUsesDurableResultAndPublishesForFollow(t *testing.T) {
	ctx, dsn := quoteTestDB(t)
	research, err := jobs.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer research.Close()
	paper, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer paper.Close()
	account, _, err := paper.CreateAccount(ctx, ledger.CreateAccount{ID: testAccountID, InitialCashUSD: "1000"})
	if err != nil {
		t.Fatal(err)
	}
	instrument := ledger.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 2}
	asOf, err := research.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	config := paperapply.SnapshotContext{
		AccountID: account.ID, SnapshotVersion: account.Version,
		Policy: paperrisk.Policy{
			Version: "quote-tape-test", MaxOrderUSD: "120", MinFreeCashUSD: "100",
			MaxPositionBPS: 2500, MaxGroupBPS: 5000, MaxGrossBPS: 9000, MaxFeeBPS: 30, MaxMarkAgeSeconds: 60,
			Instruments: []paperrisk.InstrumentRule{{Instrument: instrument, Symbol: "TSLA", Group: "tech"}},
		},
		Marks: []paperrisk.Mark{{Instrument: instrument, PriceUSD: "20", At: asOf, AvailableAt: asOf}},
	}
	portfolio, err := paperapply.Snapshot(account, config, asOf)
	if err != nil {
		t.Fatal(err)
	}
	request := paperapply.Request{Version: paperapply.Version, JobID: testJobID, SnapshotContext: config}
	snapshot := testJSON(t, portfolio)
	_, _, err = research.Create(ctx, jobs.CreateJob{
		ID: testJobID, SnapshotID: portfolio.ID, Snapshot: snapshot,
		Request: testJSON(t, map[string]any{
			"job_schema_version": "0.1", "job_id": testJobID, "portfolio": portfolio,
			"requested_symbols": []string{"TSLA"}, "policy_version": config.Policy.Version, "max_llm_calls": 1,
			"data_manifest": map[string]string{"ref": "fixture.json", "sha256": strings.Repeat("a", 64)},
		}),
		PaperDispatch: &jobs.PaperDispatchInput{AccountID: account.ID, Request: testJSON(t, request)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tape := quoteTape{Version: "0.1", Source: papersim.Source, JobID: testJobID, Scope: tapeScope{instrument, "BUY", "500"}, Observations: []observation{}}
	dir := t.TempDir()
	quoteDir := filepath.Join(dir, "quotes")
	if err := os.Mkdir(quoteDir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(quoteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	invoke := func(tape quoteTape) (pinResult, string, error) {
		raw := testJSON(t, tape)
		digest := sha256Hex(raw)
		path := filepath.Join(dir, "tape.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"--job", testJobID, "--tape", path, "--sha256", digest, "--quote-root", quoteDir}
		var output bytes.Buffer
		if err := run(ctx, dsn, args, &output); err != nil {
			return pinResult{}, digest, err
		}
		var result pinResult
		if err := contract.Decode(output.Bytes(), contract.PaperQuoteTapeReceipt, &result); err != nil {
			t.Fatal(err)
		}
		return result, digest, nil
	}
	if pending, _, err := invoke(tape); err != nil || pending.Status != "awaiting_research" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	assertNoPin(t, root)

	lease, err := research.Claim(ctx, "quote-test", "44444444-4444-4444-8444-444444444444", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const report, evidenceID = "local fixture decision", "TSLA:final_trade_decision"
	batch := contract.Batch{
		Version: "0.1", Mode: "research", RunID: "55555555-5555-4555-8555-555555555555", SnapshotID: portfolio.ID,
		AsOf: asOf, Created: asOf, Expires: asOf.Add(time.Minute),
		Evidence: map[string]string{evidenceID: sha256Hex([]byte(report))},
		Intents: []contract.Intent{{
			Symbol: "TSLA", Action: "BUY", Notional: "100", ID: testDecision, Confidence: new(0.75), Expected: new(2.0),
			Horizon: contract.Horizon{Min: 1, Max: 24}, Thesis: []string{"fixture"}, Invalidation: []string{"fixture"},
			Evidence: []string{evidenceID}, CostBPS: 30,
		}},
	}
	receipt, _, err := research.Complete(ctx, jobs.CompleteJob{
		JobID: testJobID, WorkerID: lease.WorkerID, Token: lease.Token, ExpiresAt: batch.Expires,
		Payload: testJSON(t, map[string]any{
			"result_schema_version": "0.1", "batch": batch,
			"reports":  map[string]map[string]string{"TSLA": {"final_trade_decision": report}},
			"metadata": map[string]any{"source_mode": "synthetic_fixture", "alpha_measured": false, "llm_calls": 0},
		}),
	}, func(jobs.Job, time.Time) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	base := papersim.Quote{
		Version: "0.1", Source: papersim.Source, Instrument: instrument, Side: "BUY", QuantityRaw: "500",
		GrossUSD: "100", FeeBPS: new(20), FixedFeeUSD: "0", ExpiresAt: batch.Expires, Execution: "fill",
	}
	at := receipt.CompletedAt
	makeObservation := func(quote papersim.Quote, delta, lag time.Duration, firmness string) observation {
		quote.ObservedAt = at.Add(delta)
		return observation{quote.ObservedAt.Add(lag), firmness, testJSON(t, quote)}
	}
	wrongIdentity, soft, expensive, expired, wrongSize := base, base, base, base, base
	wrongIdentity.Instrument.Decimals = 1
	expensive.GrossUSD = "110"
	expired.ExpiresAt = at.Add(4 * time.Microsecond)
	wrongSize.QuantityRaw = "499"
	tape.Observations = []observation{
		makeObservation(base, 0, time.Microsecond, "firm"),
		makeObservation(soft, time.Microsecond, 0, "indicative"),
		makeObservation(wrongIdentity, 2*time.Microsecond, 0, "firm"),
		makeObservation(expensive, 3*time.Microsecond, 0, "firm"),
		makeObservation(expired, 4*time.Microsecond-time.Nanosecond, 0, "firm"),
		makeObservation(wrongSize, 4*time.Microsecond, 0, "firm"),
		makeObservation(base, 30*time.Second, 0, "firm"),
	}
	if pending, _, err := invoke(tape); err != nil || pending.Status != "awaiting_quote" {
		t.Fatalf("no eligible quote = %+v, %v", pending, err)
	}
	assertNoPin(t, root)
	earliest := makeObservation(base, 6*time.Microsecond, time.Microsecond, "firm")
	tape.Observations = append(tape.Observations, makeObservation(base, 5*time.Microsecond, 5*time.Microsecond, "firm"), earliest)
	first, digest, err := invoke(tape)
	if err != nil || first.Status != "pinned" || first.CompletedAt == nil || first.CheckedAt == nil || first.AvailableAt == nil ||
		first.ObservedAt == nil || !first.CompletedAt.Equal(at) || !first.AvailableAt.Equal(earliest.Available) ||
		!first.ObservedAt.After(at) || !first.AvailableAt.Before(*first.CheckedAt) || first.Quote == nil {
		t.Fatalf("first pin = %+v, %v", first, err)
	}
	pin, err := readPin(root, testJobID)
	if err != nil || pin.Quote != *first.Quote || pin.Quote.Ref != quoteFilename(digest, sha256Hex(earliest.Raw)) {
		t.Fatalf("published pin = %+v, %v", pin, err)
	}
	loaded, err := readRoot(root, pin.Quote.Ref, papersim.MaxInputBytes)
	if err != nil || !bytes.Equal(loaded, earliest.Raw) {
		t.Fatalf("quote bytes changed: %v", err)
	}
	if replay, _, err := invoke(tape); err != nil || replay.Status != "replayed" || replay.Quote == nil || *replay.Quote != pin.Quote {
		t.Fatalf("replayed pin = %+v, %v", replay, err)
	}
	if expiredReplay, err := pinCompleted(ctx, testClock{research, batch.Expires.Add(time.Minute)}, root, tape, digest, nil); err != nil || expiredReplay.Status != "replayed" {
		t.Fatalf("replay after expiry = %+v, %v", expiredReplay, err)
	}
	tape.Observations = tape.Observations[1:]
	if _, _, err := invoke(tape); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("changed tape result = %v", err)
	}
	entries, err := os.ReadDir(quoteDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("replay wrote additional files: %v, %v", entries, err)
	}
	followed, err := paperapply.Follow(ctx, research, paper, request, func(_ context.Context, jobID string) ([]byte, error) {
		pin, err := readPin(root, jobID)
		if err != nil {
			return nil, err
		}
		return readRoot(root, pin.Quote.Ref, papersim.MaxInputBytes)
	}, *first.CheckedAt)
	if err != nil || followed.Status != "applied" || followed.Application == nil || followed.Application.Execution == nil ||
		followed.Application.Execution.QuoteSHA256 != pin.Quote.SHA256 || followed.Application.Execution.Order.Status != "filled" {
		t.Fatalf("Follow pinned tape = %+v, %v", followed, err)
	}
}

func TestConcurrentPublicationKeepsOneImmutablePin(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	type outcome struct {
		pin     quotePin
		body    []byte
		created bool
		err     error
	}
	results := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, body := range [][]byte{[]byte(`{"first":true}`), []byte(`{"first":false}`)} {
		go func() {
			digest := sha256Hex(body)
			pin := quotePin{"0.1", testJobID, pinQuote{quoteFilename(digest, digest), digest}}
			ready.Done()
			ready.Wait()
			created, err := publishPin(t.Context(), root, pin, body)
			results <- outcome{pin, body, created, err}
		}()
	}
	first, second := <-results, <-results
	if !first.created {
		first, second = second, first
	}
	if !first.created || first.err != nil || second.created || !errors.Is(second.err, ledger.ErrConflict) {
		t.Fatalf("publication race = %+v and %+v", first, second)
	}
	pin, err := readPin(root, testJobID)
	if err != nil || *pin != first.pin {
		t.Fatalf("winner pin = %+v, %v", pin, err)
	}
	body, err := readRoot(root, pin.Quote.Ref, papersim.MaxInputBytes)
	if err != nil || !bytes.Equal(body, first.body) {
		t.Fatalf("winning bytes = %s, %v", body, err)
	}
	if _, err := readRoot(root, second.pin.Quote.Ref, papersim.MaxInputBytes); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("losing bytes still present: %v", err)
	}
}

type testClock struct {
	*jobs.Store
	at time.Time
}

func (c testClock) Now(context.Context) (time.Time, error) { return c.at, nil }

func assertNoPin(t *testing.T, root *os.Root) {
	t.Helper()
	if _, err := readPin(root, testJobID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected pin or error: %v", err)
	}
}

func testJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func quoteTestDB(t *testing.T) (context.Context, string) {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("test database must be a PostgreSQL URI")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	t.Cleanup(cancel)
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test database failed")
	}
	schema := "quote_tape_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer base.Close(cleanup)
		if _, err := base.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	params := parsed.Query()
	params.Set("search_path", schema)
	parsed.RawQuery = params.Encode()
	return ctx, parsed.String()
}
