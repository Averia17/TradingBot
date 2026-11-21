package paper_quotes

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_sim"
)

func TestOwnerIdentityTapeVerifiesExactFirmMembership(t *testing.T) {
	root, err := os.OpenRoot(filepath.Join("..", "..", "..", "..", "contracts"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	const name = "valid-paper-identity-quote-tape.json"
	raw, err := root.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var document tapeDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	digest, registry := sumHex(raw), document.IdentityRegistrySHA256
	tape, err := OpenIdentityTape(root, name, digest, document.AccountID, registry)
	if err != nil {
		t.Fatal(err)
	}
	original := document.Observations[0].Raw
	available := document.Observations[0].AvailableAt
	completion := available.Add(-2 * time.Second)
	now := available.Add(time.Second)
	deadline := available.Add(time.Minute)
	match, err := tape.Verify(original, completion, now, deadline)
	if err != nil || !bytes.Equal(match.Raw, original) || !match.AvailableAt.Equal(available) ||
		match.Quote.SecurityID != "fixture-tesla" || match.Quote.IdentityRegistrySHA256 != registry || match.SHA256 != sumHex(original) {
		t.Fatalf("firm observation = %+v, %v", match, err)
	}
	match.Raw[0] = '!'
	*match.Quote.FeeBPS = 5000
	again, err := tape.Verify(original, completion, now, deadline)
	if err != nil || !bytes.Equal(again.Raw, original) || *again.Quote.FeeBPS != 0 {
		t.Fatalf("returned observation changed the immutable tape: %+v, %v", again, err)
	}
	for _, tc := range []struct {
		name            string
		raw             []byte
		completion, now time.Time
		deadline        time.Time
		want            error
	}{
		{"different whitespace", append(bytes.Clone(original), '\n'), completion, now, deadline, ErrQuoteNotInTape},
		{"observed at completion", original, match.Quote.ObservedAt, now, deadline, ErrQuoteUnavailable},
		{"still unavailable", original, completion, available, deadline, ErrQuoteUnavailable},
		{"quote expired", original, completion, match.Quote.ExpiresAt, deadline, ErrQuoteUnavailable},
		{"application expired", original, completion, now, now, ErrQuoteUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tape.Verify(tc.raw, tc.completion, tc.now, tc.deadline); !errors.Is(err, tc.want) {
				t.Fatalf("verification error = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := OpenIdentityTape(root, name, digest, document.AccountID, strings.Repeat("a", 64)); err == nil {
		t.Fatal("opened tape using the wrong independently pinned registry")
	}
	if _, err := OpenIdentityTape(root, name, digest, "33333333-3333-4333-8333-333333333333", registry); err == nil {
		t.Fatal("opened owner tape for a different account")
	}
	if _, err := OpenIdentityTape(root, name, strings.Repeat("a", 64), document.AccountID, registry); err == nil {
		t.Fatal("opened tape using the wrong archive digest")
	}
	changed := bytes.Replace(raw, []byte(`"firmness": "firm"`), []byte(`"firmness": "indicative"`), 1)
	soft, err := DecodeTape(changed, sumHex(changed), "", document.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := soft.Verify(original, completion, now, deadline); !errors.Is(err, ErrQuoteUnavailable) {
		t.Fatalf("indicative observation verification error = %v", err)
	}
	mixed := bytes.Replace(raw, []byte(registry), []byte(strings.Repeat("a", 64)), 1)
	if _, err := DecodeTape(mixed, sumHex(mixed), "", document.AccountID); err == nil {
		t.Fatal("accepted quote with a different registry than its tape")
	}
}

func TestSelectChecksOnlyThroughTheEarliestApprovedQuote(t *testing.T) {
	now := time.Date(2026, 1, 3, 15, 0, 30, 0, time.UTC)
	completedAt := now.Add(-time.Minute)
	quote := horizonQuote(now)
	quote.ExpiresAt = now.Add(time.Minute)
	makeObservation := func(offset time.Duration) tapeRecord {
		quote.ObservedAt = completedAt.Add(offset)
		return tapeRecord{AvailableAt: quote.ObservedAt, Firmness: "firm", Raw: horizonJSON(t, quote)}
	}
	late := makeObservation(3 * time.Second)
	first := makeObservation(time.Second)
	second := makeObservation(2 * time.Second)
	tape := horizonTape(t, quote.IdentityRegistrySHA256, late, second, first, makeObservation(0))
	calls := 0
	selected := tape.Select(completedAt, now, quote.ExpiresAt, func(raw []byte, at time.Time) error {
		calls++
		if !at.Equal(now) {
			t.Fatalf("authorization cutoff = %v", at)
		}
		switch {
		case calls == 1 && bytes.Equal(raw, first.Raw):
			return errors.New("first eligible quote exceeds policy")
		case calls == 2 && bytes.Equal(raw, second.Raw):
			return nil
		default:
			t.Fatalf("unexpected authorization call %d", calls)
			return nil
		}
	})
	if selected == nil || !bytes.Equal(selected.Raw, second.Raw) || calls != 2 {
		t.Fatalf("selected %+v after %d checks", selected, calls)
	}
}

func TestHorizonExitRequiresExactKnownFullSale(t *testing.T) {
	cutoff := time.Date(2026, 1, 3, 15, 0, 30, 0, time.UTC)
	base := horizonQuote(cutoff)
	for _, tc := range []struct {
		name string
		edit func(*paper_sim.Quote, *tapeRecord)
	}{
		{"different security at same ticker and tuple", func(q *paper_sim.Quote, _ *tapeRecord) { q.SecurityID = "old-tesla" }},
		{"different instrument", func(q *paper_sim.Quote, _ *tapeRecord) { q.Instrument.ID += "-other" }},
		{"different registry version", func(q *paper_sim.Quote, _ *tapeRecord) { q.Instrument.RegistryVersion = "fixture-v2" }},
		{"different decimals", func(q *paper_sim.Quote, _ *tapeRecord) { q.Instrument.Decimals++ }},
		{"smaller size", func(q *paper_sim.Quote, _ *tapeRecord) { q.QuantityRaw = "499" }},
		{"larger size", func(q *paper_sim.Quote, _ *tapeRecord) { q.QuantityRaw = "501" }},
		{"buy", func(q *paper_sim.Quote, _ *tapeRecord) { q.Side = "BUY" }},
		{"indicative", func(_ *paper_sim.Quote, r *tapeRecord) { r.Firmness = "indicative" }},
		{"expired at cutoff", func(q *paper_sim.Quote, _ *tapeRecord) { q.ExpiresAt = cutoff }},
		{"learned after cutoff", func(_ *paper_sim.Quote, r *tapeRecord) { r.AvailableAt = cutoff.Add(time.Nanosecond) }},
		{"observed after cutoff", func(q *paper_sim.Quote, r *tapeRecord) {
			q.ObservedAt, r.AvailableAt = cutoff.Add(time.Nanosecond), cutoff.Add(time.Nanosecond)
		}},
		{"stale despite recent availability", func(q *paper_sim.Quote, _ *tapeRecord) {
			q.ObservedAt = cutoff.Add(-5*time.Second - time.Nanosecond)
		}},
		{"partial fill", func(q *paper_sim.Quote, _ *tapeRecord) { q.Execution, q.ExecutedQuantityRaw = "partial", "250" }},
		{"timeout", func(q *paper_sim.Quote, _ *tapeRecord) { q.Execution = "timeout" }},
		{"reject", func(q *paper_sim.Quote, _ *tapeRecord) { q.Execution = "reject" }},
		{"zero gross", func(q *paper_sim.Quote, _ *tapeRecord) { q.GrossUSD = "0" }},
		{"fee exceeds gross", func(q *paper_sim.Quote, _ *tapeRecord) { q.FixedFeeUSD = "100.01" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quote := base
			record := tapeRecord{AvailableAt: cutoff, Firmness: "firm"}
			tc.edit(&quote, &record)
			record.Raw = horizonJSON(t, quote)
			tape := horizonTape(t, quote.IdentityRegistrySHA256, record)
			if got, found, err := tape.HorizonExit(base.SecurityID, base.Instrument, base.QuantityRaw, cutoff, 5*time.Second); err != nil || found {
				t.Fatalf("unexpected horizon sale: %+v, %v", got, err)
			}
		})
	}
	valid := horizonTape(t, base.IdentityRegistrySHA256, tapeRecord{cutoff, "firm", horizonJSON(t, base)})
	got, found, err := valid.HorizonExit(base.SecurityID, base.Instrument, base.QuantityRaw, cutoff, 5*time.Second)
	if err != nil || !found || got.Quote.Instrument != base.Instrument || !got.AvailableAt.Equal(cutoff) ||
		!bytes.Equal(got.Raw, horizonJSON(t, base)) || got.SHA256 != sumHex(got.Raw) {
		t.Fatalf("expected exact observation known at cutoff and observed at maximum age: %+v, %t, %v", got, found, err)
	}
}

func TestHorizonExitRejectsAmbiguousBestObservation(t *testing.T) {
	cutoff := time.Date(2026, 1, 3, 15, 0, 30, 0, time.UTC)
	base := horizonQuote(cutoff)
	conflicting, newer := base, base
	conflicting.GrossUSD = "110"
	newer.ObservedAt = cutoff.Add(-time.Second)
	one := tapeRecord{cutoff.Add(-2 * time.Second), "firm", horizonJSON(t, base)}
	two := tapeRecord{cutoff.Add(-3 * time.Second), "firm", horizonJSON(t, conflicting)}
	three := tapeRecord{cutoff, "firm", horizonJSON(t, newer)}
	for _, records := range [][]tapeRecord{{one, two}, {two, one}} {
		tape := horizonTape(t, base.IdentityRegistrySHA256, records...)
		if _, found, err := tape.HorizonExit(base.SecurityID, base.Instrument, base.QuantityRaw, cutoff, time.Minute); found || !errors.Is(err, ErrAmbiguousHorizonQuote) {
			t.Fatalf("ambiguous horizon result: %t, %v", found, err)
		}
	}
	for _, records := range [][]tapeRecord{{one, two, three}, {three, two, one}} {
		tape := horizonTape(t, base.IdentityRegistrySHA256, records...)
		got, found, err := tape.HorizonExit(base.SecurityID, base.Instrument, base.QuantityRaw, cutoff, time.Minute)
		if err != nil || !found || !bytes.Equal(got.Raw, three.Raw) {
			t.Fatalf("latest quote should win over superseded ambiguity: %+v, %t, %v", got, found, err)
		}
	}
	duplicate := tapeRecord{cutoff, "firm", one.Raw}
	tape := horizonTape(t, base.IdentityRegistrySHA256, duplicate, one)
	got, found, err := tape.HorizonExit(base.SecurityID, base.Instrument, base.QuantityRaw, cutoff, time.Minute)
	if err != nil || !found || !got.AvailableAt.Equal(one.AvailableAt) {
		t.Fatalf("identical quote should preserve its first availability: %+v, %t, %v", got, found, err)
	}
}

func TestHorizonExitRejectsLegacyTapeAndInvalidWindow(t *testing.T) {
	cutoff := time.Date(2026, 1, 3, 15, 0, 30, 0, time.UTC)
	quote := horizonQuote(cutoff)
	bound := horizonTape(t, quote.IdentityRegistrySHA256, tapeRecord{cutoff, "firm", horizonJSON(t, quote)})
	const accountID = "11111111-1111-4111-8111-111111111111"
	legacyRaw := horizonJSON(t, tapeDocument{
		Version: "0.2", Source: paper_sim.Source, AccountID: accountID, Observations: []tapeRecord{},
	})
	legacy, err := DecodeTape(legacyRaw, sumHex(legacyRaw), "", accountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tape     *Tape
		quantity string
		at       time.Time
		maxAge   time.Duration
	}{
		{legacy, "500", cutoff, time.Minute},
		{bound, "0", cutoff, time.Minute},
		{bound, "0500", cutoff, time.Minute},
		{bound, "500", time.Time{}, time.Minute},
		{bound, "500", cutoff, 0},
	} {
		if _, found, err := tc.tape.HorizonExit(quote.SecurityID, quote.Instrument, tc.quantity, tc.at, tc.maxAge); err == nil || found {
			t.Fatal("horizon exit accepted an unbound tape or invalid measurement window")
		}
	}
}

func horizonQuote(cutoff time.Time) paper_sim.Quote {
	return paper_sim.Quote{
		Version: paper_sim.IdentityQuoteVersion, Source: paper_sim.Source,
		SecurityID: "fixture-tesla", IdentityRegistrySHA256: strings.Repeat("e", 64),
		Instrument: ledger.Instrument{ID: "fixture:TSLA", RegistryVersion: "fixture-v1", Decimals: 2},
		Side:       "SELL", QuantityRaw: "500", GrossUSD: "100", FeeBPS: new(12), FixedFeeUSD: "0",
		ObservedAt: cutoff.Add(-5 * time.Second), ExpiresAt: cutoff.Add(time.Minute), Execution: "fill",
	}
}

func horizonTape(t *testing.T, registry string, records ...tapeRecord) *Tape {
	t.Helper()
	const accountID = "11111111-1111-4111-8111-111111111111"
	raw := horizonJSON(t, tapeDocument{
		Version: IdentityTapeVersion, Source: paper_sim.Source, AccountID: accountID,
		IdentityRegistrySHA256: registry, Observations: records,
	})
	tape, err := DecodeTape(raw, sumHex(raw), "", accountID)
	if err != nil {
		t.Fatal(err)
	}
	return tape
}

func horizonJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
