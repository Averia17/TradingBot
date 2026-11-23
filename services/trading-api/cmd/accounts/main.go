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

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/accounts_http"
	"tradingbot/trading-api/internal/paper_check"
)

func main() {
	address := flag.String("listen", "127.0.0.1:8080", "HTTP address for the paper accounts API")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *address); err != nil {
		slog.Error("paper accounts stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, address string) error {
	token, err := accounts_http.InternalToken()
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("paper accounts requires an internal token")
	}
	outboxToken, err := accounts_http.OutboxToken()
	if err != nil {
		return err
	}
	if outboxToken == token {
		return errors.New("paper accounts outbox requires a separate token")
	}
	startup, cancel := context.WithTimeout(ctx, time.Minute)
	dsn := os.Getenv("TRADINGBOT_ACCOUNTS_DATABASE_URL")
	store, err := accounts.Open(startup, dsn)
	cancel()
	if err != nil {
		return errors.New("paper accounts storage startup failed; check PostgreSQL configuration")
	}
	defer store.Close()

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	mux := http.NewServeMux()
	mux.Handle(paper_check.AccountsPath, paper_check.NewAccountsHandler(dsn, token))
	mux.Handle("/", accounts_http.NewHandler(store, token, accounts.NewOutboxHandler(store, outboxToken)))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	slog.Info("paper accounts API listening", "address", listener.Addr())
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
