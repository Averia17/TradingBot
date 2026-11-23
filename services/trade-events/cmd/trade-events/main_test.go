package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPaperSourceRequiresOutboxCredential(t *testing.T) {
	t.Setenv("TRADINGBOT_ACCOUNTS_URL", "http://127.0.0.1:8080")
	t.Setenv("TRADINGBOT_PAPER_INTERNAL_TOKEN", "test-order-worker-token")
	t.Setenv("TRADINGBOT_ACCOUNTS_OUTBOX_TOKEN", "")
	t.Setenv("TRADINGBOT_ACCOUNTS_OUTBOX_TOKEN_FILE", "")
	if source, err := loadPaperSource(); err == nil {
		source.Close()
		t.Fatal("relay accepted the general paper worker token")
	}

	path := filepath.Join(t.TempDir(), "outbox_token")
	if err := os.WriteFile(path, []byte("test-outbox-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRADINGBOT_ACCOUNTS_OUTBOX_TOKEN_FILE", path)
	source, err := loadPaperSource()
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	t.Setenv("TRADINGBOT_ACCOUNTS_OUTBOX_TOKEN", "test-second-outbox-token")
	if _, err := loadPaperSource(); err == nil {
		t.Fatal("relay accepted two outbox token sources")
	}
}
