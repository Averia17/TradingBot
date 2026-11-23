package paper_memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/paper_risk"
)

const baseArchive = `{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[{"kind":"bar","provider":"fixture","record_id":"close","symbol":"ABC","revision":1,"event_time":"2026-11-16T14:00:00Z","available_at":"2026-11-16T14:00:00Z","ingested_at":"2026-11-16T14:00:00Z","payload":{"close":"5.00000001"}}]}`

func archiveDigest(raw []byte) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func archivePolicy() paper_risk.Policy {
	return paper_risk.Policy{
		Version: "paper-v1", MaxOrderUSD: "500", MinFreeCashUSD: "10",
		MaxPositionBPS: 5000, MaxGroupBPS: 8000, MaxGrossBPS: 9000,
		MaxFeeBPS: 100, MaxMarkAgeSeconds: 60,
		Instruments: []paper_risk.InstrumentRule{{Instrument: testInstrument, Symbol: "ABC", Group: "fixture-tech"}},
	}
}

func TestExportArchiveBoundsExactLedgerOutcomesAtTheReceipt(t *testing.T) {
	history := eventHistory{account(t), reserve(t, 2, 2, "BUY", "7", "70.00000001", "0.00000007"),
		fill(t, 3, 2, 3, "70.00000001", "0.00000007", testTime.Add(2*time.Minute))}
	for i := 0; i < 7; i++ {
		id := 4 + 2*i
		history = append(history,
			reserve(t, id, int64(id), "SELL", "1", "7.00000001", "0.00000001"),
			fill(t, id+1, id, int64(id+1), "7.00000001", "0.00000001", testTime.Add(time.Duration(id)*time.Minute)),
		)
	}
	base := []byte(baseArchive)
	for _, test := range []struct {
		name   string
		asOf   time.Time
		count  int
		lastID int
	}{
		{"before first sell receipt", history[4].RecordedAt.Add(-time.Nanosecond), 0, 0},
		{"at first sell receipt", history[4].RecordedAt, 1, 5},
		{"after last sell receipt", history[16].RecordedAt, MaxArchiveOutcomesPerSymbol, 17},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := ExportArchive(context.Background(), history, testAccount, test.asOf, archivePolicy(), base, archiveDigest(base))
			if err != nil {
				t.Fatal(err)
			}
			var archive archiveDocument
			if err := json.Unmarshal(raw, &archive); err != nil || len(archive.Records) != 1+test.count ||
				archive.SourceMode != "synthetic_fixture" || string(archive.Records[0]) != `{"kind":"bar","provider":"fixture","record_id":"close","symbol":"ABC","revision":1,"event_time":"2026-11-16T14:00:00Z","available_at":"2026-11-16T14:00:00Z","ingested_at":"2026-11-16T14:00:00Z","payload":{"close":"5.00000001"}}` {
				t.Fatalf("unexpected preserved archive: %+v / %v", archive, err)
			}
			if test.count == 0 {
				if archive.Version != "0.1" {
					t.Fatal("an archive without new records changed version")
				}
				return
			}
			if archive.Version != "0.2" {
				t.Fatal("factual paper records require an archive that supports them")
			}
			var recent paperArchiveRecord
			if err := json.Unmarshal(archive.Records[1], &recent); err != nil {
				t.Fatal(err)
			}
			if recent.Kind != "paper_outcome" || recent.Provider != "paper-ledger" || recent.Symbol != "ABC" ||
				recent.RecordID != testAccount+"-"+eventID(test.lastID) || recent.Payload.AccountVersion != int64(test.lastID) ||
				recent.Payload.SourceMode != "paper_simulated_ledger" || recent.Payload.RiskGroup != "fixture-tech" ||
				recent.Payload.FeeUSD != "0.00000001" || recent.Payload.QuantityRaw != "1" ||
				!recent.AvailableAt.Equal(test.asOf) || !recent.IngestedAt.Equal(recent.AvailableAt) || !recent.EventTime.Before(recent.AvailableAt) {
				t.Fatalf("wrong paper provenance: %+v", recent)
			}
			if test.lastID == 5 && (recent.Payload.CostBasisUSD != "10.00000001" || recent.Payload.NetPnLUSD != "-3.00000001") {
				t.Fatalf("rounded or discarded negative outcome: %+v", recent.Payload)
			}
			validatePublishedOutcome(t, recent.Payload)
		})
	}
}

func validatePublishedOutcome(t *testing.T, payload archivedOutcome) {
	t.Helper()
	const base = "https://tradingbot.local/contracts/paper-outcome.schema.json"
	source, err := os.ReadFile("../../../../contracts/paper-outcome.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(base, value); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(base)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded archivedOutcome
	if err := contract.Decode(raw, schema, &decoded); err != nil {
		t.Fatalf("Go producer differs from the published paper outcome schema: %v", err)
	}
}

func TestExportRejectsTamperedBaseAndUntrustedPaperRecords(t *testing.T) {
	base := []byte(baseArchive)
	tainted := []byte(`{"schema_version":"0.1","source_mode":"synthetic_fixture","records":[{"kind":"paper_outcome","provider":"pretend"}]}`)
	for _, test := range []struct {
		raw    []byte
		digest string
	}{
		{base, archiveDigest(tainted)},
		{tainted, archiveDigest(tainted)},
	} {
		if _, err := ExportArchive(context.Background(), eventHistory{account(t)}, testAccount, testTime.Add(time.Hour), archivePolicy(), test.raw, test.digest); err == nil {
			t.Fatalf("accepted invalid base archive: %s", test.raw)
		}
	}
}

func TestExportUsesTheExactRegistryIdentity(t *testing.T) {
	history := eventHistory{
		account(t), reserve(t, 2, 2, "BUY", "1", "12", "0"), fill(t, 3, 2, 3, "12", "0", testTime.Add(2*time.Minute)),
		reserve(t, 4, 4, "SELL", "1", "14", "0"), fill(t, 5, 4, 5, "14", "0", testTime.Add(4*time.Minute)),
	}
	policy := archivePolicy()
	policy.Instruments[0].Instrument.RegistryVersion = "another-v1"
	raw := []byte(baseArchive)
	body, err := ExportArchive(context.Background(), history, testAccount, testTime.Add(time.Hour), policy, raw, archiveDigest(raw))
	if err != nil || string(body) != string(raw) {
		t.Fatalf("a mismatched registry version must not publish memory, got %s / %v", body, err)
	}
}

func TestDurableArchiveRejectsALessonWithDifferentFIFOAccounting(t *testing.T) {
	history := eventHistory{
		account(t), reserve(t, 2, 2, "BUY", "1", "12", "0"), fill(t, 3, 2, 3, "12", "0", testTime.Add(2*time.Minute)),
		reserve(t, 4, 4, "SELL", "1", "14", "0"), fill(t, 5, 4, 5, "14", "0", testTime.Add(4*time.Minute)),
	}
	asOf := testTime.Add(time.Hour)
	fact, err := BuildLesson(context.Background(), history, testAccount, eventID(5), asOf)
	if err != nil {
		t.Fatal(err)
	}
	fact.CostBasisUSD, fact.NetPnLUSD = "13", "1"
	lookup := func(_ context.Context, _, _ string, _ time.Time) (accounts.LessonEvent, error) {
		return accounts.LessonEvent{Action: "record", AvailableAt: asOf, Fact: fact}, nil
	}
	base := []byte(baseArchive)
	if _, err := exportArchive(context.Background(), history, lookup, testAccount, asOf, archivePolicy(), base, archiveDigest(base)); err == nil {
		t.Fatal("archive accepted a different net amount and cost basis for the same fill")
	}
}
