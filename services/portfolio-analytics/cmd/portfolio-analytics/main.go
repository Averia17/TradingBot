package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"tradingbot/portfolio-analytics/internal/analytics"
)

func main() {
	if err := run(); err != nil {
		slog.Error("portfolio analytics stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	archive := flag.String("publish-valuation", "", "publish a validated JSON valuation archive to Kafka and exit")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	accountID := os.Getenv("TRADINGBOT_ANALYTICS_ACCOUNT_ID")
	if !analytics.ValidAccountID(accountID) {
		return errors.New("TRADINGBOT_ANALYTICS_ACCOUNT_ID must be a canonical account UUID")
	}
	brokers := strings.Split(os.Getenv("TRADINGBOT_ANALYTICS_BROKERS"), ",")
	for i, broker := range brokers {
		brokers[i] = strings.TrimSpace(broker)
		if brokers[i] == "" {
			return errors.New("TRADINGBOT_ANALYTICS_BROKERS must contain Kafka broker addresses")
		}
	}
	if *archive != "" {
		count, hash, err := analytics.PublishArchive(ctx, brokers, accountID, *archive)
		if err == nil {
			slog.Info("valuation archive acknowledged by Kafka", "count", count, "sha256", hash)
		}
		return err
	}
	return serve(ctx, brokers, accountID)
}

func serve(ctx context.Context, brokers []string, accountID string) error {
	markAge, err := time.ParseDuration(envOr("TRADINGBOT_ANALYTICS_MARK_MAX_AGE", "72h"))
	if err != nil || markAge <= 0 || markAge > 7*24*time.Hour {
		return errors.New("TRADINGBOT_ANALYTICS_MARK_MAX_AGE must be a positive duration up to 168h")
	}
	policy := analytics.Policy{MarkMaxAge: markAge, HeadMaxAge: 2 * time.Minute,
		BenchmarkID: strings.TrimSpace(os.Getenv("TRADINGBOT_ANALYTICS_BENCHMARK_ID"))}
	start, cancelStart := context.WithTimeout(ctx, 8*time.Second)
	store, err := analytics.Open(start, os.Getenv("TRADINGBOT_ANALYTICS_DATABASE_URL"))
	cancelStart()
	if err != nil {
		return err
	}
	defer store.Close()

	consumerCtx, stopConsumer := context.WithCancel(ctx)
	consumerDone := make(chan struct{})
	var streamReady atomic.Bool
	group := envOr("TRADINGBOT_ANALYTICS_GROUP", "tradingbot-portfolio-analytics-"+accountID)
	go func() {
		defer close(consumerDone)
		for consumerCtx.Err() == nil {
			err := analytics.Consume(consumerCtx, store, brokers, group, accountID, func() { streamReady.Store(true) })
			streamReady.Store(false)
			if err != nil {
				slog.Error("portfolio Kafka consumer stopped", "error", err)
			}
			select {
			case <-consumerCtx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}()
	server := &http.Server{
		Addr:              envOr("TRADINGBOT_ANALYTICS_HTTP_ADDR", ":8080"),
		Handler:           analytics.HTTPHandler(store, accountID, policy, streamReady.Load),
		ReadHeaderTimeout: 4 * time.Second,
		ReadTimeout:       6 * time.Second,
		WriteTimeout:      8 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-serverDone:
	}
	stopConsumer()
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdown)
	<-consumerDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return shutdownErr
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
