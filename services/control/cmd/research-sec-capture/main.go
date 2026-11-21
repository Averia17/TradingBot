// Command research-sec-capture archives public SEC observations without a model.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var issuers = map[string]string{
	"NVDA": "0001045810", "TSLA": "0001318605", "AAPL": "0000320193", "MSFT": "0000789019",
}

type record struct {
	Kind        string         `json:"kind"`
	Provider    string         `json:"provider"`
	ID          string         `json:"record_id"`
	Symbol      string         `json:"symbol"`
	Revision    int            `json:"revision"`
	EventTime   time.Time      `json:"event_time"`
	AvailableAt time.Time      `json:"available_at"`
	IngestedAt  time.Time      `json:"ingested_at"`
	Payload     map[string]any `json:"payload"`
}

type archive struct {
	Version string   `json:"schema_version"`
	Source  string   `json:"source_mode"`
	Records []record `json:"records"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "SEC capture failed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("research-sec-capture", flag.ContinueOnError)
	symbols := flags.String("symbols", "NVDA,TSLA,AAPL,MSFT", "comma-separated supported US equity symbols")
	agent := flags.String("user-agent", os.Getenv("TRADINGBOT_SEC_USER_AGENT"), "truthful SEC User-Agent identifying the requesting organization/contact")
	path := flags.String("out", "", "new archive JSON file; omitted writes the archive to stdout")
	cutoff := flags.String("as-of", "", "UTC cutoff in RFC3339; defaults to capture start")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*agent) == "" || strings.ContainsAny(*agent, "\r\n\x00") {
		return errors.New("provide a truthful --user-agent or TRADINGBOT_SEC_USER_AGENT")
	}
	asOf := time.Now().UTC()
	if *cutoff != "" {
		parsed, err := time.Parse(time.RFC3339Nano, *cutoff)
		if err != nil || parsed.IsZero() || parsed.After(asOf) {
			return errors.New("--as-of must be an available RFC3339 cutoff")
		}
		asOf = parsed.UTC()
	}
	selected := strings.Split(*symbols, ",")
	seen := map[string]bool{}
	for _, symbol := range selected {
		if issuers[symbol] == "" || seen[symbol] {
			return errors.New("symbols must be unique members of NVDA,TSLA,AAPL,MSFT")
		}
		seen[symbol] = true
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || (req.URL.Host != "data.sec.gov" && req.URL.Host != "www.sec.gov" && req.URL.Host != "sec.gov") {
			return errors.New("SEC redirect is outside the supported public hosts")
		}
		return nil
	}}
	source := collector{client: client, userAgent: *agent, dataRoot: "https://data.sec.gov", archiveRoot: "https://www.sec.gov", now: time.Now}
	result := archive{Version: "0.1", Source: "local_archive", Records: []record{}}
	for _, symbol := range selected {
		rows, err := source.capture(ctx, symbol, asOf)
		if err != nil {
			return fmt.Errorf("%s: %w", symbol, err)
		}
		result.Records = append(result.Records, rows...)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if *path == "" {
		_, err = output.Write(raw)
		return err
	}
	if err := publish(*path, raw); err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	return json.NewEncoder(output).Encode(map[string]any{
		"archive_path": *path, "sha256": hex.EncodeToString(digest[:]), "records": len(result.Records), "source_mode": result.Source,
	})
}

func publish(path string, raw []byte) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	name := ".sec-capture-" + rand.Text()
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	_, writeErr := file.Write(raw)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	return root.Link(name, filepath.Base(path))
}
