package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tradingbot/control/internal/paperthesis"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout)
	cancel()
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("paper shadow outcome report failed", "error", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-shadow-outcome", flag.ContinueOnError)
	request := flags.String("input", "", "local shadow outcome request file")
	digest := flags.String("sha256", "", "SHA-256 of the exact request bytes")
	entry := flags.String("entry", "", "independently pinned synthetic BUY observation file")
	exit := flags.String("exit", "", "independently pinned synthetic SELL observation file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *request == "" || *digest == "" || *entry == "" || *exit == "" || dsn == "" {
		return errors.New("provide --input, --sha256, --entry, --exit and TRADINGBOT_DATABASE_URL")
	}
	raw, err := readLocal(*request)
	if err != nil {
		return err
	}
	buy, err := readLocal(*entry)
	if err != nil {
		return err
	}
	sell, err := readLocal(*exit)
	if err != nil {
		return err
	}
	input, err := paperthesis.ParseShadowInput(raw, *digest, buy, sell)
	if err != nil {
		return err
	}
	report, err := paperthesis.ReadShadowReport(ctx, dsn, input)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}

func readLocal(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > paperthesis.MaxShadowInputBytes {
		return nil, errors.New("paper shadow input must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open paper shadow input failed")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, paperthesis.MaxShadowInputBytes+1))
	if err != nil {
		return nil, errors.New("read paper shadow input failed")
	}
	return raw, nil
}
