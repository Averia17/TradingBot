package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../shared/trade-intent/valid-batch.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPythonBatch(t *testing.T) {
	b, err := ParseBatch(fixture(t), time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if b.Intents[0].Notional != "0" {
		t.Fatal("amount lost")
	}
	bookData, err := os.ReadFile("../../../shared/trade-intent/valid-portfolio.json")
	if err != nil {
		t.Fatal(err)
	}
	book, err := ParsePortfolio(bookData)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckAllocation(b, book, []string{"TSLA", "NVDA"}); err != nil {
		t.Fatal(err)
	}
}

func TestPortfolioRequiresSerializedDefaultFields(t *testing.T) {
	data, err := os.ReadFile("../../../shared/trade-intent/valid-portfolio.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"currency", "cash_asset", "reserved_cash_usd", "max_position_pct", "positions",
	} {
		t.Run(name, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, name)
			mutated, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParsePortfolio(mutated); err == nil {
				t.Fatal("accepted incomplete portfolio")
			}
		})
	}
}

func TestRejectWireMutations(t *testing.T) {
	cases := [][2]string{
		{`"notional_usd": "0"`, `"notional_usd": 0`},
		{`"notional_usd": "0"`, `"notional_usd": "0.000000001"`},
		{`"notional_usd": "0"`, `"notional_usd": "1"`},
		{`"notional_usd": "0"`, `"notional_usd": "-1"`},
		{`"schema_version": "0.1"`, `"schema_version": "0.2"`},
		{`"execution_mode": "research"`, `"execution_mode": "live"`},
		{`"min_hours": 24`, `"min_hours": 800`},
		{`"max_execution_cost_bps": 10`, `"max_execution_cost_bps": 1001`},
		{`"confidence": null`, `"confidence": 1.01`},
		{`"action": "HOLD"`, `"action": "BUY"`},
		{`"TSLA:market_report"`, `"missing:evidence"`},
		{`"symbol": "NVDA"`, `"symbol": "TSLA"`},
		{`"expires_at": "2026-10-04T12:30:00Z"`, `"expires_at": "2026-10-04T12:00:00Z"`},
	}
	for _, c := range cases {
		t.Run(c[1], func(t *testing.T) {
			b := bytes.Replace(fixture(t), []byte(c[0]), []byte(c[1]), 1)
			if bytes.Equal(b, fixture(t)) {
				t.Fatal("mutation did not apply")
			}
			if _, err := ParseBatch(b, time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC), time.Hour); err == nil {
				t.Fatal("accepted invalid batch")
			}
		})
	}
	for _, data := range [][]byte{
		append(fixture(t), []byte(` {}`)...),
		bytes.Replace(fixture(t), []byte(`"schema_version": "0.1",`), []byte(`"schema_version":"0.1","schema_version":"0.1",`), 1),
		bytes.Replace(fixture(t), []byte(`"run_id":`), []byte(`"withdraw_to":"attacker","run_id":`), 1),
	} {
		if _, err := ParseBatch(data, time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC), time.Hour); err == nil {
			t.Fatal("accepted ambiguous JSON")
		}
	}
}

func TestResearchNumberBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
		valid bool
	}{
		{"finite estimate at float64 limit", `"expected_return_pct": null`, "1.7976931348623157e308", true},
		{"subnormal confidence", `"confidence": null`, "5e-324", true},
		{"float overflow", `"expected_return_pct": null`, "1.8e308", false},
		{"integer overflow", `"min_hours": 24`, "9223372036854775808", false},
		{"fractional integer", `"min_hours": 24`, "24.5", false},
		{"negative underflow remains negative for range checks", `"confidence": null`, "-1e-325", false},
		{"exponent outside the wire bound", `"expected_return_pct": null`, "1e-401", false},
	}
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			field, _, _ := strings.Cut(test.field, ":")
			data := bytes.Replace(fixture(t), []byte(test.field), []byte(field+": "+test.value), 1)
			if _, err := ParseBatch(data, now, time.Hour); (err == nil) != test.valid {
				t.Fatalf("ParseBatch error = %v, want valid = %t", err, test.valid)
			}
		})
	}
}

func TestRejectDuplicateDecisionIDWithDifferentCase(t *testing.T) {
	data := bytes.ReplaceAll(
		fixture(t),
		[]byte("00000000-0000-0000-0000-000000000003"),
		[]byte("00000000-0000-0000-0000-00000000000A"),
	)
	data = bytes.ReplaceAll(
		data,
		[]byte("00000000-0000-0000-0000-000000000004"),
		[]byte("00000000-0000-0000-0000-00000000000a"),
	)
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	if _, err := ParseBatch(data, now, time.Hour); err == nil {
		t.Fatal("accepted duplicate decision UUID")
	}
}

func TestAllocationMatchesSnapshotUUIDRegardlessOfCase(t *testing.T) {
	data := bytes.ReplaceAll(
		fixture(t),
		[]byte("00000000-0000-0000-0000-000000000001"),
		[]byte("00000000-0000-0000-0000-00000000000A"),
	)
	now := time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC)
	b, err := ParseBatch(data, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bookData, err := os.ReadFile("../../../shared/trade-intent/valid-portfolio.json")
	if err != nil {
		t.Fatal(err)
	}
	bookData = bytes.ReplaceAll(
		bookData,
		[]byte("00000000-0000-0000-0000-000000000001"),
		[]byte("00000000-0000-0000-0000-00000000000a"),
	)
	book, err := ParsePortfolio(bookData)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckAllocation(b, book, []string{"TSLA", "NVDA"}); err != nil {
		t.Fatal(err)
	}
}

func TestExpiryAndStale(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC),
		time.Date(2026, 10, 4, 11, 59, 0, 0, time.UTC),
	} {
		if _, err := ParseBatch(fixture(t), now, time.Hour); err == nil {
			t.Fatal("accepted expiry/future")
		}
	}
	if _, err := ParseBatch(fixture(t), time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC), time.Second); err == nil {
		t.Fatal("accepted stale snapshot")
	}
}
