package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/paper_risk"
)

func TestLessonCommandsUsePostgreSQL(t *testing.T) {
	base := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dsn, err := url.Parse(base)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal("connect to test database")
	}
	schema := "paper_lesson_cli_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer conn.Close(cleanup)
		if _, err := conn.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	params := dsn.Query()
	params.Set("search_path", schema)
	dsn.RawQuery = params.Encode()
	store, err := accounts.Open(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := func(n int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", n) }
	account := id(1)
	if _, _, err := store.CreateAccount(ctx, accounts.CreateAccount{ID: account, InitialCashUSD: "1000"}); err != nil {
		t.Fatal(err)
	}
	settle := func(orderN int, version int64, side, gross string) {
		t.Helper()
		order, _, err := store.Reserve(ctx, accounts.ReservationRequest{
			AccountID: account, OrderID: id(orderN), DecisionID: id(orderN + 10), QuoteID: id(orderN + 20),
			SnapshotVersion: version, Instrument: accounts.Instrument{ID: "fixture:ABC", RegistryVersion: "fixture-v1", Decimals: 2},
			Side: side, QuantityRaw: "1", GrossUSD: gross, MaxFeeUSD: "0.00000001", ExpiresAt: time.Now().Add(time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Resolve(ctx, accounts.ResolutionRequest{
			AccountID: account, OrderID: order.OrderID, EventID: id(orderN + 1), Outcome: "filled",
			GrossUSD: gross, FeeUSD: "0.00000001", ObservedAt: order.CreatedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	settle(2, 1, "BUY", "10.00000001")
	settle(4, 3, "SELL", "10.00000003")

	invoke := func(args ...string) ([]byte, error) {
		var result bytes.Buffer
		err := run(ctx, dsn.String(), args, &result)
		return result.Bytes(), err
	}
	runCommand := func(args ...string) []byte {
		t.Helper()
		result, err := invoke(args...)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	raw := runCommand("record-lesson", "--account", account, "--fill-event", id(5), "--event", id(6))
	var receipt accounts.LessonEvent
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Fact.NetPnLUSD != "0" || !receipt.Fact.ExitFeeErasedGain {
		t.Fatalf("unexpected recorded lesson: %s, %v", raw, err)
	}
	retraction := runCommand("correct-lesson", "--account", account, "--lesson", receipt.LessonID, "--event", id(7), "--action", "retract", "--expected-revision", "1")
	prior := runCommand("lessons", "--account", account, "--lesson", receipt.LessonID, "--as-of", receipt.AvailableAt.Format(time.RFC3339Nano))
	history := runCommand("lessons", "--account", account, "--lesson", receipt.LessonID, "--history")
	var earlier, events []json.RawMessage
	if json.Unmarshal(prior, &earlier) != nil || len(earlier) != 1 || !bytes.Equal(earlier[0], bytes.TrimSpace(raw)) ||
		json.Unmarshal(history, &events) != nil || len(events) != 2 || !bytes.Equal(events[0], bytes.TrimSpace(retraction)) {
		t.Fatalf("unexpected cutoff or history output: %s / %s", prior, history)
	}
	validateMemorySchema(t, "paper-memory-lesson.schema.json", raw, retraction)
	var withdrawn accounts.LessonEvent
	if err := json.Unmarshal(retraction, &withdrawn); err != nil {
		t.Fatal(err)
	}
	assertDurableArchive(t, invoke, receipt, withdrawn)
}

func assertDurableArchive(t *testing.T, invoke func(...string) ([]byte, error), recorded, withdrawn accounts.LessonEvent) {
	t.Helper()
	directory := t.TempDir()
	market := []byte(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[]}`)
	marketPath, policyPath := filepath.Join(directory, "market.json"), filepath.Join(directory, "policy.json")
	if err := os.WriteFile(marketPath, market, 0600); err != nil {
		t.Fatal(err)
	}
	policy := paper_risk.Policy{
		Version: "paper-v1", MaxOrderUSD: "500", MinFreeCashUSD: "10",
		MaxPositionBPS: 5000, MaxGroupBPS: 8000, MaxGrossBPS: 9000,
		MaxFeeBPS: 100, MaxMarkAgeSeconds: 60,
		Instruments: []paper_risk.InstrumentRule{{Instrument: recorded.Fact.Instrument, Symbol: "ABC", Group: "fixture-tech"}},
	}
	policyRaw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, policyRaw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		cutoff     time.Time
		durable    bool
		wantLesson string
		invalid    bool
	}{
		{"source-only", recorded.Fact.SourceAvailableAt, true, "", false},
		{"recorded", recorded.AvailableAt, true, recorded.LessonID, false},
		{"withdrawn", withdrawn.AvailableAt, true, "", false},
		{"legacy", withdrawn.AvailableAt.Add(time.Nanosecond), false, "", false},
		{"submicrosecond", recorded.AvailableAt.Add(time.Nanosecond), true, "", true},
	} {
		outputPath := filepath.Join(directory, test.name+".json")
		args := []string{"--account", recorded.Fact.AccountID, "--as-of", test.cutoff.Format(time.RFC3339Nano),
			"--input", marketPath, "--input-sha256", fmt.Sprintf("%x", sha256.Sum256(market)), "--policy", policyPath, "--output", outputPath}
		if test.durable {
			args = append(args, "--durable-lessons")
		}
		_, err := invoke(args...)
		if test.invalid {
			if _, fileErr := os.Stat(outputPath); !errors.Is(err, accounts.ErrInvalid) || !errors.Is(fileErr, os.ErrNotExist) {
				t.Fatalf("invalid archive cutoff was accepted: %v / %v", err, fileErr)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		archive, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Records []struct {
				Payload json.RawMessage `json:"payload"`
			} `json:"records"`
		}
		if err := json.Unmarshal(archive, &document); err != nil || len(document.Records) != 1 {
			t.Fatalf("archive must retain the verified FIFO outcome for %s: %s, %v", test.name, archive, err)
		}
		payload := document.Records[0].Payload
		if !test.durable {
			validateMemorySchema(t, "paper-outcome.schema.json", payload)
			continue
		}
		validateMemorySchema(t, "paper-durable-outcome.schema.json", payload)
		var result struct {
			NetPnL string `json:"net_pnl_usd"`
			Memory struct {
				AsOf   time.Time `json:"as_of"`
				Active *struct {
					LessonID string `json:"lesson_id"`
					EventID  string `json:"event_id"`
					Hash     string `json:"source_outcome_sha256"`
				} `json:"active"`
			} `json:"durable_memory"`
		}
		if err := json.Unmarshal(payload, &result); err != nil || result.NetPnL != recorded.Fact.NetPnLUSD || !result.Memory.AsOf.Equal(test.cutoff) {
			t.Fatalf("cutoff/FIFO facts changed for %s: %s, %v", test.name, payload, err)
		}
		active := result.Memory.Active
		if test.wantLesson == "" && active != nil || test.wantLesson != "" &&
			(active == nil || active.LessonID != test.wantLesson || active.EventID != recorded.EventID || active.Hash != recorded.Fact.SourceOutcomeSHA256) {
			t.Fatalf("wrong durable lesson for %s: %s", test.name, payload)
		}
	}
}

func validateMemorySchema(t *testing.T, filename string, samples ...[]byte) {
	t.Helper()
	file, err := os.Open(filepath.Join("../../../../contracts", filename))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	document, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	name := "https://tradingbot.local/contracts/" + filename
	if err := compiler.AddResource(name, document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range samples {
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(sample))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatal(err)
		}
	}
}
