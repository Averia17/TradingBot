package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_workflow"
)

func TestJupiterFixtureEndpointIsNumericLoopbackOnly(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:18085", "http://[::1]:18085/"} {
		client, err := newJupiterFixtureClient(origin)
		if err != nil || !strings.HasSuffix(client.endpoint, "/v1/paper-fixture/settle") {
			t.Fatalf("rejected fixture origin %q", origin)
		}
	}
	for _, origin := range []string{
		"http://localhost:18085", "http://192.0.2.1:18085", "http://127.0.0.1", "http://127.0.0.1:0",
		"https://127.0.0.1:18085", "http://user@127.0.0.1:18085", "http://127.0.0.1:18085/foo",
		"http://127.0.0.1:18085?route=mainnet", "http://127.0.0.1:18085#fragment",
	} {
		if _, err := newJupiterFixtureClient(origin); err == nil {
			t.Fatalf("accepted fixture route override %q", origin)
		}
	}
}

func TestFixtureSettlementChecksBothPlannedAndReservedGross(t *testing.T) {
	for _, test := range []struct {
		side, actual, planned, limit string
		allowed                      bool
	}{
		{"BUY", "99.99999999", "100", "100", true},
		{"BUY", "99.99999999", "100", "99.99999998", false},
		{"BUY", "100.00000001", "100", "110", false},
		{"SELL", "215", "210", "210", true},
		{"SELL", "215", "210", "216", false},
		{"SELL", "215", "216", "210", false},
		{"SELL", "0", "0", "0", false},
	} {
		if favorableFixtureGross(test.side, test.actual, test.planned, test.limit) != test.allowed {
			t.Fatalf("unexpected gross authorization for %+v", test)
		}
	}
}

func TestFixtureSettlementRecoveryBindsPersistedFill(t *testing.T) {
	order := ledger.Order{
		ReservationRequest: ledger.ReservationRequest{Side: "SELL", QuantityRaw: "100", GrossUSD: "210"},
		Status:             "filled",
		Fills:              []ledger.Fill{{EventID: "event", QuantityRaw: "100", GrossUSD: "215", FeeUSD: "0"}},
	}
	resolution := ledger.ResolutionRequest{EventID: "event", GrossUSD: "210", FeeUSD: "0.00000000"}
	fill, err := priorFixtureFill(order, resolution)
	if err != nil || fill.GrossUSD != "215" {
		t.Fatalf("could not recover matching settled economics: %+v / %v", fill, err)
	}
	resolution.EventID = "other-event"
	if _, err := priorFixtureFill(order, resolution); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("conflicting settled event accepted: %v", err)
	}
	order.Status = "unknown"
	order.RemainingQuantityRaw = "50"
	if _, err := priorFixtureFill(order, resolution); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("partial reservation accepted as a whole fill: %v", err)
	}
}

func TestJupiterFixtureSettlementBindsFinalEvidence(t *testing.T) {
	digest := strings.Repeat("a", 64)
	base := `"schema_version":"0.1","source_mode":"synthetic_fixture","request_sha256":"` + digest + `"`
	const final = `,"status":"finalized","quantity_raw":"100","gross_usd":"215","fee_usd":"0","observed_at":"2026-11-22T10:00:00.123456Z"}`
	start := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	request := jupiterFixtureRequest{Side: "SELL", QuantityRaw: "100", GrossUSD: "210", GrossLimitUSD: "210", FeeUSD: "0", CreatedAt: start, ExpiresAt: start.Add(time.Minute)}
	for _, test := range []struct {
		name, body string
		status     int
		pending    bool
		valid      bool
	}{
		{"final proof", `{` + base + final, http.StatusOK, false, true},
		{"gross replay normalization", `{` + base + strings.ReplaceAll(final, `"215"`, `"215.00000000"`), http.StatusOK, false, true},
		{"pending proof", `{` + base + `,"status":"pending"}`, http.StatusAccepted, true, false},
		{"pending with null fill", `{` + base + `,"status":"pending","gross_usd":null}`, http.StatusAccepted, false, false},
		{"wrong HTTP status", `{` + base + final, http.StatusAccepted, false, false},
		{"wrong request hash", `{` + strings.ReplaceAll(base, digest, strings.Repeat("b", 64)) + final, http.StatusOK, false, false},
		{"worse gross", `{` + base + strings.ReplaceAll(final, `"215"`, `"209"`), http.StatusOK, false, false},
		{"fee changed", `{` + base + strings.ReplaceAll(final, `"fee_usd":"0"`, `"fee_usd":"1"`), http.StatusOK, false, false},
		{"noncanonical timestamp", `{` + base + strings.ReplaceAll(final, `.123456Z`, `.123456+00:00`), http.StatusOK, false, false},
		{"submicrosecond timestamp", `{` + base + strings.ReplaceAll(final, `.123456Z`, `.123456001Z`), http.StatusOK, false, false},
		{"expired timestamp", `{` + base + strings.ReplaceAll(final, `10:00:00.123456Z`, `10:01:00Z`), http.StatusOK, false, false},
		{"duplicate key", `{` + base + `,"schema_version":"0.1"` + final, http.StatusOK, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed, err := parseJupiterFixtureSettlement([]byte(test.body), test.status, digest, request)
			if test.valid != (err == nil && !observed.ObservedAt.IsZero() && observed.GrossUSD == "215") ||
				test.pending != errors.Is(err, paper_workflow.ErrSettlementPending) || (!test.valid && err == nil) {
				t.Fatalf("unexpected proof result: %v, %v", observed, err)
			}
		})
	}
}
