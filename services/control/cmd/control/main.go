package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tradingbot/control/internal/control"
	"tradingbot/control/internal/jobs"
	"tradingbot/control/internal/ledger"
)

func main() {
	address := flag.String("listen", "127.0.0.1:8080", "HTTP address for the local research control API")
	maxResearchAge := flag.Duration("max-research-age", time.Hour, "maximum age of a research snapshot for validation, creation, or lease")
	flag.Parse()
	if *maxResearchAge <= 0 {
		slog.Error("-max-research-age must be positive")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *address, *maxResearchAge, os.Getenv("TRADINGBOT_DATABASE_URL")); err != nil {
		slog.Error("control stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, address string, maxResearchAge time.Duration, dsn string) error {
	handler := control.NewHandler(maxResearchAge, time.Now, nil)
	if dsn != "" {
		startup, cancel := context.WithTimeout(ctx, 10*time.Second)
		store, err := jobs.Open(startup, dsn)
		cancel()
		if err != nil {
			return errors.New("research job storage startup failed; check database configuration and connectivity")
		}
		defer store.Close()
		paperStartup, stopPaperStartup := context.WithTimeout(ctx, 10*time.Second)
		paper, err := ledger.Open(paperStartup, dsn)
		stopPaperStartup()
		if err != nil {
			return errors.New("paper storage startup failed; check database configuration and connectivity")
		}
		defer paper.Close()
		handler = control.NewHandlerWithPaper(maxResearchAge, time.Now, store, paper)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}
	slog.Info("research control listening", "address", listener.Addr())
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
}
