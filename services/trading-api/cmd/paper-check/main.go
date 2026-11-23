package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tradingbot/trading-api/internal/paper_check"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	code := paper_check.Run(ctx, os.Args[1:], os.Stdout)
	cancel()
	stop()
	os.Exit(code)
}
