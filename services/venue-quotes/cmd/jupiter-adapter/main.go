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

	"tradingbot/venue-quotes/internal/jupiter"
	"tradingbot/venue-quotes/internal/jupiter_execution"
	"tradingbot/venue-quotes/internal/jupiter_http"
	"tradingbot/venue-quotes/internal/solana_balance"
)

const balanceTTL = 5 * time.Second

type config struct {
	apiKey        string
	listen        string
	wallet        string
	rpcURL        string
	prepareSwap   bool
	executeSwap   bool
	executionDB   string
	policyHash    string
	approvalKey   string
	executorToken string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		slog.Error("Jupiter Adapter stopped", "error", err)
		os.Exit(1)
	}
}

func readConfig() (config, error) {
	cfg := config{
		apiKey: strings.TrimSpace(os.Getenv("TRADINGBOT_JUPITER_API_KEY")),
		listen: strings.TrimSpace(os.Getenv("TRADINGBOT_JUPITER_LISTEN")),
		wallet: strings.TrimSpace(os.Getenv("TRADINGBOT_JUPITER_WALLET")),
		rpcURL: strings.TrimSpace(os.Getenv("TRADINGBOT_SOLANA_RPC_URL")),
	}
	if cfg.listen == "" {
		cfg.listen = "127.0.0.1:18084"
	}
	switch os.Getenv("TRADINGBOT_JUPITER_ENABLE_PREPARE") {
	case "true":
		cfg.prepareSwap = true
	case "", "false":
	default:
		return cfg, errors.New("TRADINGBOT_JUPITER_ENABLE_PREPARE must be true or false")
	}
	switch os.Getenv("TRADINGBOT_JUPITER_ENABLE_EXECUTE") {
	case "true":
		cfg.executeSwap = true
	case "", "false":
	default:
		return cfg, errors.New("TRADINGBOT_JUPITER_ENABLE_EXECUTE must be true or false")
	}
	if cfg.wallet == "" && (cfg.rpcURL != "" || cfg.prepareSwap) {
		return cfg, errors.New("TRADINGBOT_JUPITER_WALLET is required for balance reads or swap preparation")
	}
	if cfg.wallet != "" && !solana_balance.ValidAddress(cfg.wallet) {
		return cfg, errors.New("TRADINGBOT_JUPITER_WALLET must be a Solana public address")
	}
	if cfg.executeSwap {
		cfg.executionDB = os.Getenv("TRADINGBOT_JUPITER_SUBMISSION_DATABASE_URL")
		cfg.approvalKey = os.Getenv("TRADINGBOT_JUPITER_APPROVAL_PUBLIC_KEY")
		cfg.policyHash = os.Getenv("TRADINGBOT_JUPITER_POLICY_SHA256")
		cfg.executorToken = os.Getenv("TRADINGBOT_JUPITER_EXECUTOR_TOKEN")
		if cfg.rpcURL == "" || cfg.apiKey == "" || cfg.executionDB == "" || cfg.approvalKey == "" || cfg.policyHash == "" ||
			len(cfg.executorToken) < 32 || len(cfg.executorToken) > 256 || strings.ContainsAny(cfg.executorToken, " \t\r\n") {
			return cfg, errors.New("Jupiter execution requires RPC, API key, attempt database, approval key, policy hash and executor token")
		}
		host, _, err := net.SplitHostPort(cfg.listen)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return cfg, errors.New("Jupiter execution requires a loopback HTTP listen address")
		}
	}
	return cfg, nil
}

func run(ctx context.Context) error {
	cfg, err := readConfig()
	if err != nil {
		return err
	}
	market := jupiter.New(cfg.apiKey)
	defer market.CloseIdleConnections()
	var balances jupiter_http.BalanceProvider
	var chainStatus jupiter_http.ChainStatusProvider
	var walletRPC *solana_balance.Client
	if cfg.rpcURL != "" {
		client, err := solana_balance.New(cfg.rpcURL, cfg.wallet, balanceTTL)
		if err != nil {
			return errors.New("invalid TRADINGBOT_SOLANA_RPC_URL configuration")
		}
		defer client.CloseIdleConnections()
		balances = client
		chainStatus = client
		walletRPC = client
	}
	var prepareOwner string
	if cfg.prepareSwap {
		prepareOwner = cfg.wallet
	}
	var executor jupiter_http.SubmissionService
	if cfg.executeSwap {
		key, err := jupiter_execution.DecodeApprovalPublicKey(cfg.approvalKey)
		if err != nil {
			return errors.New("invalid TRADINGBOT_JUPITER_APPROVAL_PUBLIC_KEY")
		}
		openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		store, err := jupiter_execution.OpenPostgres(openCtx, cfg.executionDB)
		cancel()
		if err != nil {
			return errors.New("Jupiter execution attempt database unavailable")
		}
		defer store.Close()
		executor, err = jupiter_execution.New(store, market, walletRPC, cfg.wallet, cfg.policyHash, key)
		if err != nil {
			return errors.New("invalid Jupiter execution policy configuration")
		}
	}
	handler := jupiter_http.Handler(market, balances, prepareOwner)
	jupiter_http.RegisterTransactions(handler, chainStatus, executor, cfg.executorToken)

	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 3 * time.Second, WriteTimeout: 15 * time.Second,
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
	slog.Info("Jupiter Adapter listening", "address", listener.Addr().String())
	err = server.Serve(listener)
	if !stopShutdown() {
		<-done
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
