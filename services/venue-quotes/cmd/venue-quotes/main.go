package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tradingbot/venue-quotes/internal/binance"
	"tradingbot/venue-quotes/internal/dzengi"
	"tradingbot/venue-quotes/internal/jupiter"
	"tradingbot/venue-quotes/internal/quote_http"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		slog.Error("venue quote service stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	name := os.Getenv("TRADINGBOT_QUOTE_PROVIDER")
	if name != "" && name != "jupiter" && name != "dzengi" && name != "binance" {
		return errors.New("unsupported TRADINGBOT_QUOTE_PROVIDER")
	}
	var provider quote_http.Provider
	if name == "" || name == "jupiter" {
		client := jupiter.New(strings.TrimSpace(os.Getenv("TRADINGBOT_JUPITER_API_KEY")))
		defer client.CloseIdleConnections()
		provider = client
	}
	handler := quote_http.Handler(provider)
	if name == "" || name == "dzengi" {
		client := dzengi.New()
		defer client.CloseIdleConnections()
		quote_http.RegisterOrderBook(handler, "/v1/order-book", client)
	}
	if name == "" || name == "binance" {
		client := binance.New()
		defer client.CloseIdleConnections()
		quote_http.RegisterOrderBook(handler, "/v1/order-book/binance", client)
	}

	address := os.Getenv("TRADINGBOT_QUOTE_LISTEN")
	if address == "" {
		address = "127.0.0.1:18083"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 3 * time.Second, WriteTimeout: 10 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 * 1024,
	}
	done := make(chan struct{})
	stopShutdown := context.AfterFunc(ctx, func() {
		defer close(done)
		shutdown, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
	})
	slog.Info("venue quote service listening", "address", listener.Addr().String())
	err = server.Serve(listener)
	if !stopShutdown() {
		<-done
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
