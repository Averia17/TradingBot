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
	"tradingbot/trading-api/internal/jobs"
	"tradingbot/trading-api/internal/jobs_http"
	"tradingbot/trading-api/internal/paper_check"
	"tradingbot/trading-api/internal/trading_api"
)

func main() {
	address := flag.String("listen", "127.0.0.1:8080", "HTTP address for the local trading API")
	maxResearchAge := flag.Duration("max-research-age", time.Hour, "maximum age of a research snapshot for validation, creation, or lease")
	flag.Parse()
	if *maxResearchAge <= 0 {
		slog.Error("-max-research-age must be positive")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *address, *maxResearchAge, os.Getenv("TRADINGBOT_DATABASE_URL"),
		os.Getenv("TRADINGBOT_RESEARCH_IDENTITY_REGISTRY_SHA256")); err != nil {
		slog.Error("trading API stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, address string, maxResearchAge time.Duration, dsn, identityRegistrySHA256 string) error {
	var store *jobs.Store
	var paper trading_api.PaperAccountStore
	fixtureDSN := os.Getenv("TRADINGBOT_OFFLINE_TEST_DATABASE_URL")
	accountsURL := os.Getenv("TRADINGBOT_ACCOUNTS_URL")
	token, err := accounts_http.InternalToken()
	if err != nil {
		return err
	}
	if fixtureDSN != "" {
		if accountsURL != "" || dsn != "" && dsn != fixtureDSN {
			return errors.New("offline fixture storage cannot be combined with service storage")
		}
		dsn = fixtureDSN
		startup, cancel := context.WithTimeout(ctx, time.Minute)
		paperStore, err := accounts.Open(startup, fixtureDSN)
		cancel()
		if err != nil {
			return errors.New("offline fixture account storage startup failed")
		}
		defer paperStore.Close()
		paper = paperStore
	}
	if accountsURL != "" {
		accountClient, err := accounts_http.NewClient(accountsURL, token)
		if err != nil {
			return err
		}
		defer accountClient.Close()
		paper = accountClient
	}
	if dsn != "" {
		startup, cancel := context.WithTimeout(ctx, 10*time.Second)
		store, err = jobs.Open(startup, dsn)
		cancel()
		if err != nil {
			return errors.New("research job storage startup failed; check database configuration and connectivity")
		}
		defer store.Close()
	}
	handler, err := trading_api.NewHandlerWithResearchIdentity(maxResearchAge, time.Now, nil, paper, identityRegistrySHA256)
	if store != nil {
		handler, err = trading_api.NewHandlerWithResearchIdentity(maxResearchAge, time.Now, store, paper, identityRegistrySHA256)
	}
	if err != nil {
		return err
	}
	if store != nil && token != "" {
		internal, err := jobs_http.NewHandler(store, jobs_http.Config{
			Token: token, RegistrySHA256: identityRegistrySHA256, MaxResearchAge: maxResearchAge,
		})
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.Handle(jobs_http.PathPrefix, internal)
		mux.Handle(paper_check.JobsPath, paper_check.NewJobsHandler(dsn, token))
		mux.Handle("/", handler)
		handler = mux
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
	slog.Info("trading API listening", "address", listener.Addr())
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
