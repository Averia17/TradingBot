package main

import (
	"strings"
	"testing"
)

func TestWalletConfiguration(t *testing.T) {
	t.Setenv("TRADINGBOT_JUPITER_LISTEN", "")
	t.Setenv("TRADINGBOT_JUPITER_API_KEY", "")
	t.Setenv("TRADINGBOT_JUPITER_ENABLE_EXECUTE", "")
	for _, test := range []struct {
		name, wallet, rpcURL, prepare string
		allowed, prepareEnabled       bool
	}{
		{name: "public data by default", allowed: true},
		{name: "RPC requires wallet", rpcURL: "https://rpc.example"},
		{name: "wallet balance", wallet: "11111111111111111111111111111111", rpcURL: "https://rpc.example", allowed: true},
		{name: "preparation requires wallet", prepare: "true"},
		{name: "preparation opt-in", wallet: "11111111111111111111111111111111", prepare: "true", allowed: true, prepareEnabled: true},
		{name: "wallet with preparation disabled", wallet: "11111111111111111111111111111111", prepare: "false", allowed: true},
		{name: "ambiguous preparation flag", wallet: "11111111111111111111111111111111", prepare: "1"},
		{name: "invalid wallet", wallet: "0invalid", prepare: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("TRADINGBOT_JUPITER_WALLET", test.wallet)
			t.Setenv("TRADINGBOT_SOLANA_RPC_URL", test.rpcURL)
			t.Setenv("TRADINGBOT_JUPITER_ENABLE_PREPARE", test.prepare)
			cfg, err := readConfig()
			if (err == nil) != test.allowed {
				t.Fatalf("config allowed=%t, error=%v", test.allowed, err)
			}
			if test.allowed && (cfg.prepareSwap != test.prepareEnabled || cfg.listen != "127.0.0.1:18084") {
				t.Fatalf("prepare=%t listen=%q", cfg.prepareSwap, cfg.listen)
			}
		})
	}
}

func TestExecutionRequiresExplicitCredentialsAndLoopback(t *testing.T) {
	for key, value := range map[string]string{
		"TRADINGBOT_JUPITER_API_KEY":                 "test-key",
		"TRADINGBOT_JUPITER_LISTEN":                  "",
		"TRADINGBOT_JUPITER_ENABLE_PREPARE":          "",
		"TRADINGBOT_JUPITER_ENABLE_EXECUTE":          "true",
		"TRADINGBOT_JUPITER_WALLET":                  "BQ72nSv9f3PRyRKCBnHLVrerrv37CYTHm5h3s9VSGQDV",
		"TRADINGBOT_SOLANA_RPC_URL":                  "https://rpc.example",
		"TRADINGBOT_JUPITER_SUBMISSION_DATABASE_URL": "postgresql://localhost/test",
		"TRADINGBOT_JUPITER_APPROVAL_PUBLIC_KEY":     "test-key",
		"TRADINGBOT_JUPITER_POLICY_SHA256":           strings.Repeat("1", 64),
		"TRADINGBOT_JUPITER_EXECUTOR_TOKEN":          strings.Repeat("a", 32),
	} {
		t.Setenv(key, value)
	}
	if cfg, err := readConfig(); err != nil || !cfg.executeSwap {
		t.Fatalf("valid execution configuration rejected, %v", err)
	}
	t.Setenv("TRADINGBOT_JUPITER_LISTEN", "0.0.0.0:18084")
	if _, err := readConfig(); err == nil {
		t.Fatal("accepted a public execution bind")
	}
	t.Setenv("TRADINGBOT_JUPITER_LISTEN", "")
	t.Setenv("TRADINGBOT_JUPITER_EXECUTOR_TOKEN", "")
	if _, err := readConfig(); err == nil {
		t.Fatal("accepted execution without caller authentication")
	}
}
