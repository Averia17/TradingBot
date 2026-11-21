package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testAgent = "TradingBot offline fixture contact@example.invalid"

func testSubmissions(t *testing.T, ticker string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"cik": "0001045810", "name": "NVIDIA CORP", "tickers": []string{ticker},
		"filings": map[string]any{"recent": recentFilings{
			Accessions: []string{"0001045810-26-000005", "0001045810-26-000004", "0001045810-26-000003", "0001045810-26-000002"},
			Accepted:   []string{"2026-11-20T20:00:00Z", "2026-11-17T21:10:00Z", "2026-11-16T20:00:00Z", "2026-08-26T20:00:00Z"},
			Forms:      []string{"8-K", "8-K", "8-K", "10-Q"},
			Documents:  []string{"future.htm", "nvda-earnings.htm", "nvda-change.htm", "nvda-quarter.htm"},
			Items:      []string{"2.02,9.01", "2.02,9.01", "5.02", ""},
			ReportDate: []string{"2026-11-20", "2026-11-17", "2026-11-16", "2026-07-26"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

const testFacts = `{"cik":1045810,"facts":{"us-gaap":{
"RevenueFromContractWithCustomerIncludingAssessedTax":{"units":{"USD":[
{"accn":"0001045810-26-000002","form":"10-Q","filed":"2026-08-26","start":"2026-04-27","end":"2026-07-26","val":100000000001.23},
{"accn":"0001045810-26-000004","form":"8-K","filed":"2026-11-17","start":"2026-07-27","end":"2026-10-25","val":115800000000}]}},
"NetIncomeLoss":{"units":{"USD":[
{"accn":"0001045810-26-000002","form":"10-Q","filed":"2026-08-26","start":"2026-04-27","end":"2026-07-26","val":59688000000}]}}
}}}`

func TestCaptureBindsSECIdentityAcceptanceReleaseAndExactStatements(t *testing.T) {
	submissions := testSubmissions(t, "NVDA")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("User-Agent") != testAgent {
			t.Error("SEC request lacks the configured identification")
		}
		switch r.URL.Path {
		case "/submissions/CIK0001045810.json":
			_, _ = w.Write(submissions)
		case "/api/xbrl/companyfacts/CIK0001045810.json":
			_, _ = w.Write([]byte(testFacts))
		case "/Archives/edgar/data/1045810/000104581026000004/nvda-earnings.htm":
			_, _ = w.Write([]byte(`<html>Item 2.02 earnings release</html>`))
		case "/Archives/edgar/data/1045810/000104581026000004/0001045810-26-000004-index.html":
			_, _ = w.Write([]byte(`<table><tr><td><a href="cfo.htm">CFO commentary</a></td><td>EX-99.2</td></tr><tr><td><a href="q3fy27pr.htm">Earnings</a></td><td>EX-99.1</td></tr></table>`))
		case "/Archives/edgar/data/1045810/000104581026000004/q3fy27pr.htm":
			_, _ = w.Write([]byte(`<h1>NVIDIA quarterly results</h1><p>Revenue $115.8 billion for quarter ended October 25, 2026.</p>`))
		default:
			t.Errorf("unexpected SEC fixture request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	source := collector{client: server.Client(), userAgent: testAgent, dataRoot: server.URL, archiveRoot: server.URL, now: func() time.Time { return now }}
	rows, err := source.capture(t.Context(), "NVDA", now)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 5 || len(rows) != 4 {
		t.Fatalf("capture requests/records: %d/%d", requests.Load(), len(rows))
	}
	if rows[0].ID != "sec-earnings-0001045810-26-000004" || rows[0].Payload["release_status"] != "captured_exhibit" ||
		!strings.Contains(rows[0].Payload["release_excerpt"].(string), "$115.8 billion") ||
		!strings.Contains(rows[0].Payload["release_excerpt"].(string), "October 25, 2026") {
		t.Fatalf("new earnings release lost its source evidence: %+v", rows[0])
	}
	if rows[1].Payload["statement_period_end"] != "2026-07-26" || rows[1].Payload["companyfacts_status"] != "captured" {
		t.Fatalf("old quarterly statements were relabeled as the new earnings: %+v", rows[1])
	}
	facts := rows[1].Payload["statement_facts"].([]statementValue)
	if len(facts) != 2 || facts[1].Value != "100000000001.23" || facts[1].End != "2026-07-26" {
		t.Fatalf("facts lost their exact amounts or filing period: %+v", facts)
	}
	for _, row := range rows {
		if row.Provider != "sec-edgar" || row.Symbol != "NVDA" || row.Revision != 1 ||
			row.EventTime.After(now) || !row.AvailableAt.Equal(row.EventTime) || !row.IngestedAt.Equal(now) ||
			!strings.HasPrefix(row.Payload["source_url"].(string), server.URL+"/Archives/edgar/data/1045810/") {
			t.Fatalf("capture lost exact source identity or temporal provenance: %+v", row)
		}
	}
}

func TestCaptureRefusesTickerMismatchAndDisclosesUnavailableSources(t *testing.T) {
	for _, wrongIssuer := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable_documents", true: "wrong_issuer"}[wrongIssuer], func(t *testing.T) {
			ticker := "NVDA"
			if wrongIssuer {
				ticker = "TSLA"
			}
			submissions := testSubmissions(t, ticker)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if strings.HasPrefix(r.URL.Path, "/submissions/") {
					_, _ = w.Write(submissions)
					return
				}
				http.Error(w, "fixture unavailable", http.StatusForbidden)
			}))
			defer server.Close()
			now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
			source := collector{client: server.Client(), userAgent: testAgent, dataRoot: server.URL, archiveRoot: server.URL, now: func() time.Time { return now }}
			rows, err := source.capture(t.Context(), "NVDA", now)
			if wrongIssuer {
				if err == nil || len(rows) != 0 || calls.Load() != 1 {
					t.Fatalf("ticker mismatch escaped source admission: %v", err)
				}
				return
			}
			if err != nil || rows[0].Payload["release_status"] != "unavailable" ||
				rows[1].Payload["companyfacts_status"] != "unavailable" || rows[1].Payload["statement_facts"] != nil {
				t.Fatalf("unavailable data became a successful financial observation: %+v %v", rows, err)
			}
		})
	}
}

func TestArchivePublicationCannotReplaceExistingCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sec.json")
	if err := publish(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := publish(path, []byte("different")); err == nil {
		t.Fatal("publication replaced an existing source capture")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "first" {
		t.Fatalf("existing source bytes changed: %s %v", raw, err)
	}
	if err := run(context.Background(), []string{"--user-agent", ""}, os.Stdout); err == nil {
		t.Fatal("SEC capture accepted an unidentified automated request")
	}
}
