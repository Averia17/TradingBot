// research-news-capture writes eligible observations from official issuer feeds.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

type issuerSource struct {
	Symbol   string
	Provider string
	URL      string
	Domains  []string
}

var issuerSources = map[string]issuerSource{
	"NVDA": {"NVDA", "nvidia-newsroom", "https://nvidianews.nvidia.com/cats/press_release.xml", []string{"nvidia.com"}},
	"AAPL": {"AAPL", "apple-newsroom", "https://www.apple.com/newsroom/rss-feed.rss", []string{"apple.com"}},
	"MSFT": {"MSFT", "microsoft-source", "https://news.microsoft.com/source/topics/company-news/feed/", []string{"microsoft.com", "microsoft.ai"}},
}

type archive struct {
	Version    string          `json:"schema_version"`
	SourceMode string          `json:"source_mode"`
	Records    []archiveRecord `json:"records"`
}

type archiveRecord struct {
	Kind        string       `json:"kind"`
	Provider    string       `json:"provider"`
	RecordID    string       `json:"record_id"`
	Symbol      string       `json:"symbol"`
	Revision    int          `json:"revision"`
	EventTime   time.Time    `json:"event_time"`
	AvailableAt time.Time    `json:"available_at"`
	IngestedAt  time.Time    `json:"ingested_at"`
	Payload     newsEvidence `json:"payload"`
}

type newsEvidence struct {
	Headline      string `json:"headline"`
	Summary       string `json:"summary,omitempty"`
	URL           string `json:"url"`
	FeedURL       string `json:"feed_url"`
	Timestamp     string `json:"source_timestamp"`
	TimestampKind string `json:"timestamp_kind"`
	Updated       string `json:"source_updated_timestamp,omitempty"`
	EvidenceKind  string `json:"evidence_kind"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("research-news-capture", flag.ContinueOnError)
	symbols := flags.String("symbols", "NVDA,AAPL,MSFT", "comma-separated tickers with native issuer feeds: NVDA, AAPL, MSFT")
	limit := flags.Int("limit", 8, "maximum eligible records per issuer (1 to 20)")
	maxAge := flags.Duration("max-age", 720*time.Hour, "maximum age measured from receipt of the feed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *limit < 1 || *limit > 20 || *maxAge <= 0 {
		return errors.New("provide a positive --max-age and --limit from 1 to 20")
	}
	var selected []issuerSource
	seen := make(map[string]bool)
	for raw := range strings.SplitSeq(*symbols, ",") {
		symbol := strings.TrimSpace(raw)
		source, ok := issuerSources[symbol]
		if !ok {
			return fmt.Errorf("no verified issuer feed for %q", symbol)
		}
		if seen[symbol] {
			return fmt.Errorf("duplicate symbol %q", symbol)
		}
		seen[symbol] = true
		selected = append(selected, source)
	}
	client := &http.Client{
		Timeout: 12 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	defer client.CloseIdleConnections()
	document := archive{Version: "0.1", SourceMode: "local_archive"}
	for _, source := range selected {
		records, err := collectSource(ctx, client, source, *limit, *maxAge, time.Now)
		if err != nil {
			return fmt.Errorf("%s issuer feed %s: %w", source.Symbol, source.URL, err)
		}
		document.Records = append(document.Records, records...)
	}
	return json.NewEncoder(out).Encode(document)
}
