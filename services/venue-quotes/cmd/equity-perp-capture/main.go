// Equity-perp-capture emits one public trade[XYZ] market observation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"tradingbot/venue-quotes/internal/hyperliquid"
	"tradingbot/venue-quotes/internal/quotes"
)

func main() {
	symbol := flag.String("symbol", "", "underlying US equity symbol")
	var sizes []string
	flag.Func("quantity", "requested contracts for a book sweep; repeat up to 8 times", func(value string) error {
		if len(sizes) == 8 {
			return fmt.Errorf("too many book sweep sizes")
		}
		sizes = append(sizes, value)
		return nil
	})
	flag.Parse()
	if *symbol == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := hyperliquid.New()
	defer client.CloseIdleConnections()
	if err := capture(ctx, client, *symbol, sizes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func capture(ctx context.Context, client *hyperliquid.Client, symbol string, sizes []string) error {
	snapshot, err := client.Capture(ctx, symbol)
	if err != nil {
		return err
	}
	result := struct {
		hyperliquid.Capture
		Sweeps []quotes.BookSweep `json:"book_sweeps,omitempty"`
	}{Capture: snapshot}
	for _, size := range sizes {
		sweep, err := quotes.EstimateRoundTrip(snapshot.OrderBook, size, *snapshot.Market.SizeDecimals)
		if err != nil {
			return err
		}
		result.Sweeps = append(result.Sweeps, sweep)
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
