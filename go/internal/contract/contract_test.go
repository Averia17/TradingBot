package contract

import (
	"bytes"
	"os"
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
