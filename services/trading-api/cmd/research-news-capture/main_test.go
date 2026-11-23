package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCollectRSSUsesSourcePublicationAndIngestion(t *testing.T) {
	const rss = `<?xml version="1.0"?><rss version="2.0"><channel>
  <item><title>Older issuer update</title><link>https://www.example.com/older</link><pubDate>Tue, 17 Nov 2026 20:00:00 GMT</pubDate></item>
  <item><title>Issuer announces results</title><link>https://www.example.com/earnings</link><pubDate>Tue, 17 Nov 2026 21:20:00 GMT</pubDate><modDate>Tue, 17 Nov 2026 21:24:00 GMT</modDate><description><![CDATA[<p>Revenue was $57 billion &amp; earnings increased.</p><script>track()</script>]]></description></item>
  <item><title>Duplicate older metadata</title><link>https://www.example.com/earnings#read</link><pubDate>Tue, 17 Nov 2026 21:19:00 GMT</pubDate></item>
  <item><title>Unknown timestamp</title><link>https://www.example.com/undated</link><pubDate>Tue, 17 Nov 2026 21:20:00 XYZ</pubDate></item>
  <item><title>Foreign link</title><link>https://example.net/announcement</link><pubDate>Wed, 18 Nov 2026 18:00:00 +0000</pubDate></item>
  <item><title>Future entry</title><link>https://www.example.com/future</link><pubDate>Fri, 20 Nov 2026 21:20:00 GMT</pubDate></item>
  </channel></rss>`
	received := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("User-Agent") == "" {
			t.Errorf("missing normal feed request metadata: %s", r.Method)
		}
		_, _ = io.WriteString(w, rss)
	}))
	defer server.Close()
	source := issuerSource{Symbol: "NVDA", Provider: "fixture-issuer", URL: server.URL, Domains: []string{"example.com"}}
	rows, err := collectSource(t.Context(), server.Client(), source, 1, 48*time.Hour, func() time.Time { return received })
	if err != nil || len(rows) != 1 {
		t.Fatalf("collect RSS: %+v %v", rows, err)
	}
	want := time.Date(2026, 11, 17, 21, 20, 0, 0, time.UTC)
	record := rows[0]
	if record.Kind != "news" || record.Symbol != "NVDA" || record.Provider != "fixture-issuer" || record.Revision != 1 ||
		!record.EventTime.Equal(want) || !record.AvailableAt.Equal(want.Add(4*time.Minute)) || !record.IngestedAt.Equal(received) ||
		record.Payload.URL != "https://www.example.com/earnings" || record.Payload.Headline != "Issuer announces results" ||
		record.Payload.TimestampKind != "rss_pub_date" || record.Payload.Summary != "Revenue was $57 billion & earnings increased." ||
		record.Payload.Updated != "Tue, 17 Nov 2026 21:24:00 GMT" || record.Payload.FeedURL != server.URL || len(record.RecordID) != 64 {
		t.Fatalf("incorrect issuer RSS evidence: %+v", record)
	}
}

func TestCollectAtomPreservesAlternateLinkAndExplicitUpdate(t *testing.T) {
	const atom = `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom">
   <entry><title>Apple post</title><updated>2026-11-18T13:59:20.769Z</updated>
   <link rel="enclosure" href="https://www.example.com/image.jpg"/>
   <link href="https://www.example.com/newsroom/release/"/>
   <content type="html">&lt;p&gt;Short company update.&lt;/p&gt;</content>
   </entry></feed>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, atom) }))
	defer server.Close()
	received := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	source := issuerSource{Symbol: "AAPL", Provider: "fixture-issuer", URL: server.URL, Domains: []string{"example.com"}}
	rows, err := collectSource(t.Context(), server.Client(), source, 8, time.Hour*720, func() time.Time { return received })
	if err != nil || len(rows) != 1 {
		t.Fatalf("collect Atom: %+v %v", rows, err)
	}
	row := rows[0]
	if row.Payload.URL != "https://www.example.com/newsroom/release/" || row.Payload.TimestampKind != "atom_updated" ||
		row.Payload.Summary != "Short company update." || row.AvailableAt.Format(time.RFC3339Nano) != "2026-11-18T13:59:20.769Z" {
		t.Fatalf("incorrect Atom evidence: %+v", row)
	}
}

func TestNVIDIAFinancialReleaseCarriesBoundedPrimaryEvidence(t *testing.T) {
	const releaseTitle = "NVIDIA Announces Financial Results for Third Quarter Fiscal 2027"
	source := issuerSources["NVDA"]
	received := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	article := feedArticle{
		title: releaseTitle, link: "https://nvidianews.nvidia.com/news/fixture-results",
		timestamp: "Tue, 17 Nov 2026 21:20:00 GMT", timestampKind: "rss_pub_date", summary: "Issuer headline summary",
		content: `<p>Revenue was $100 billion.</p><p>Diluted earnings per share were $2.00.</p>` + strings.Repeat("More details. ", 300),
	}
	record, ok := buildRecord(source, article, received, received.Add(-72*time.Hour))
	if !ok || record.Kind != "filing" || record.Payload.EvidenceKind != "issuer_earnings_release" ||
		!strings.Contains(record.Payload.Summary, "Revenue was $100 billion. Diluted earnings per share were $2.00.") ||
		len([]rune(record.Payload.Summary)) > 3200 {
		t.Fatalf("earnings release lost primary financial evidence: %+v", record)
	}
	article.title = "NVIDIA Sets Conference Call for Third-Quarter Financial Results"
	record, ok = buildRecord(source, article, received, received.Add(-72*time.Hour))
	if !ok || record.Kind != "news" || record.Payload.Summary != "Issuer headline summary" {
		t.Fatalf("conference notice was mislabeled as released earnings: %+v", record)
	}
}

func TestCollectRejectsMissingOrUnusableIssuerResponses(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
	}{
		{name: "source blocked", code: http.StatusForbidden},
		{name: "unknown feed shape", code: http.StatusOK, body: `<html><body></body></html>`},
		{name: "oversized", code: http.StatusOK, body: strings.Repeat(" ", maxFeedBytes+1)},
		{name: "stale only", code: http.StatusOK, body: `<rss><channel><item><title>Aged news</title><link>https://www.example.com/aged</link><pubDate>Tue, 17 Nov 2026 00:00:00 GMT</pubDate></item></channel></rss>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.code)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			source := issuerSource{Symbol: "MSFT", Provider: "fixture-issuer", URL: server.URL, Domains: []string{"example.com"}}
			rows, err := collectSource(t.Context(), server.Client(), source, 8, 12*time.Hour, func() time.Time {
				return time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
			})
			if err == nil || len(rows) != 0 {
				t.Fatalf("unusable response yielded records: %+v %v", rows, err)
			}
		})
	}
}
