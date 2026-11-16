package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tradingbot/control/internal/researchscheduler"
)

func main() {
	var config researchscheduler.Config
	path := flag.String("config", "", "frozen research schedule JSON file")
	flag.StringVar(&config.ControlURL, "control-url", "http://127.0.0.1:18080", "loopback Control HTTP origin")
	flag.StringVar(&config.ArchiveRoot, "archive-root", "", "directory containing the frozen local archives")
	flag.DurationVar(&config.PollInterval, "poll-interval", time.Second, "maximum delay between scheduler checks")
	flag.BoolVar(&config.Once, "once", false, "submit only occurrences due at startup")
	flag.Parse()
	file, err := os.Open(*path)
	if err != nil {
		slog.Error("cannot open schedule config", "error", err)
		os.Exit(2)
	}
	schedule, err := researchscheduler.Load(file)
	file.Close()
	if err != nil {
		slog.Error("cannot load schedule config", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := researchscheduler.Run(ctx, schedule, config); err != nil {
		slog.Error("research schedule stopped", "error", err)
		os.Exit(1)
	}
}
