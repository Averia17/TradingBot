package papersim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/control/internal/ledger"
)

type recordingLedger struct {
	reserve []ledger.ReservationRequest
	resolve []ledger.ResolutionRequest
	created time.Time
}

func (l *recordingLedger) Reserve(_ context.Context, r ledger.ReservationRequest) (ledger.Order, bool, error) {
	l.reserve = append(l.reserve, r)
	return ledger.Order{ReservationRequest: r, CreatedAt: l.created, Status: "reserved"}, len(l.reserve) == 1, nil
}

func (l *recordingLedger) Resolve(_ context.Context, r ledger.ResolutionRequest) (ledger.Order, bool, error) {
	l.resolve = append(l.resolve, r)
	order := ledger.Order{ReservationRequest: l.reserve[len(l.reserve)-1], Status: r.Outcome, AccountVersion: 3, CreatedAt: l.created}
	if r.Outcome == "filled" {
		order.Fill = &ledger.Fill{EventID: r.EventID, GrossUSD: r.GrossUSD, FeeUSD: r.FeeUSD, ObservedAt: r.ObservedAt}
	}
	return order, len(l.resolve) == 1, nil
}

func TestPublishedFixtureContract(t *testing.T) {
	const base = "https://tradingbot.local/contracts/"
	const fixtureSchema = "paper-replay.schema.json"
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	for _, file := range []string{fixtureSchema, "paper-account.schema.json"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", file))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(base+file, parsed); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	in, quote := fixture(now)
	raw := bindQuote(t, &in, quote)
	result, err := Run(context.Background(), &recordingLedger{created: now}, in, raw, now)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{"Intent": in, "Quote": quote, "Result": result} {
		schema, err := compiler.Compile(base + fixtureSchema + "#/$defs/" + name)
		if err != nil {
			t.Fatal(err)
		}
		serialized, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(serialized))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(parsed); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func fixture(now time.Time) (Intent, Quote) {
	instrument := ledger.Instrument{ID: "fixture:NVDAx", RegistryVersion: "synthetic-v1", Decimals: 8}
	return Intent{
			Version: "0.1", AccountID: "11111111-1111-4111-8111-111111111111", DecisionID: "22222222-2222-4222-8222-222222222222",
			SnapshotVersion: 1, Instrument: instrument, Side: "BUY", QuantityRaw: "125000000", GrossLimitUSD: "124", MaxFeeUSD: "0.20",
			DecidedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute),
		}, Quote{
			Version: "0.1", Source: "synthetic_fixture", Instrument: instrument, Side: "BUY", QuantityRaw: "125000000",
			GrossUSD: "123.45678901", FeeBPS: new(15), FixedFeeUSD: "0.01", ObservedAt: now, ExpiresAt: now.Add(2 * time.Minute), Execution: "fill",
		}
}

func bindQuote(t *testing.T, in *Intent, q Quote) []byte {
	t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	in.QuoteSHA256 = hex.EncodeToString(digest[:])
	return raw
}

func TestReplayUsesExactFixtureAndStableIDs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	intent, quote := fixture(now)
	raw := bindQuote(t, &intent, quote)
	store := &recordingLedger{created: now}
	first, err := Run(context.Background(), store, intent, raw, now)
	if err != nil || first.Order.Status != "filled" {
		t.Fatalf("first attempt: %+v, %v", first, err)
	}
	second, err := Run(context.Background(), store, intent, raw, now.Add(3*time.Minute))
	if err != nil || second.QuoteSHA256 != intent.QuoteSHA256 {
		t.Fatalf("retry after quote expiry: %+v, %v", second, err)
	}
	if store.reserve[0] != store.reserve[1] || store.resolve[0] != store.resolve[1] {
		t.Fatalf("retry changed persisted identity or content: %+v %+v", store.reserve, store.resolve)
	}
	r, outcome := store.reserve[0], store.resolve[0]
	if !r.ExpiresAt.Equal(intent.ExpiresAt) || r.Instrument != intent.Instrument || r.GrossUSD != intent.GrossLimitUSD || r.MaxFeeUSD != intent.MaxFeeUSD ||
		outcome.GrossUSD != quote.GrossUSD || outcome.FeeUSD != "0.19518519" || !outcome.ObservedAt.Equal(now) {
		t.Fatalf("unexpected reserve or outcome: %+v %+v", r, outcome)
	}
}

func TestReplayRejectsBeforeReservation(t *testing.T) {
	now := time.Now().UTC()
	mutations := map[string]func(*Intent, *Quote){
		"other source":        func(_ *Intent, q *Quote) { q.Source = "market" },
		"instrument mismatch": func(_ *Intent, q *Quote) { q.Instrument.RegistryVersion = "v2" },
		"raw units mismatch":  func(_ *Intent, q *Quote) { q.QuantityRaw = "123" },
		"side mismatch":       func(_ *Intent, q *Quote) { q.Side = "SELL" },
		"future quote":        func(_ *Intent, q *Quote) { q.ObservedAt = now.Add(time.Second) },
		"quote before intent": func(_ *Intent, q *Quote) { q.ObservedAt = now.Add(-2 * time.Second) },
		"missing fee model":   func(_ *Intent, q *Quote) { q.FeeBPS = nil },
		"price too high":      func(i *Intent, _ *Quote) { i.GrossLimitUSD = "123" },
		"fee too high":        func(i *Intent, _ *Quote) { i.MaxFeeUSD = "0.19" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			intent, quote := fixture(now)
			mutate(&intent, &quote)
			raw := bindQuote(t, &intent, quote)
			store := &recordingLedger{}
			if _, err := Run(context.Background(), store, intent, raw, now); err == nil || len(store.reserve) != 0 {
				t.Fatalf("invalid quote reached ledger: %v, %+v", err, store.reserve)
			}
		})
	}
	intent, quote := fixture(now)
	raw := bindQuote(t, &intent, quote)
	if _, err := Run(context.Background(), &recordingLedger{}, intent, append(raw, '\n'), now); err == nil {
		t.Fatal("modified fixture bytes accepted")
	}
}
