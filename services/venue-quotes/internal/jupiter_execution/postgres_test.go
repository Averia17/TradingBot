package jupiter_execution

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	solana "github.com/solana-foundation/solana-go/v2"

	"tradingbot/venue-quotes/internal/jupiter"
	"tradingbot/venue-quotes/internal/solana_balance"
)

func TestPostgresKeepsUnknownAttemptAcrossConcurrentReplayAndRestart(t *testing.T) {
	dsn := prepareTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	store, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wallet := solana.NewWallet()
	encoded := signedTestTransaction(t, wallet)
	transaction, err := jupiter.ValidateSignedSwap(encoded, wallet.PublicKey().String())
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approval := Approval{
		SchemaVersion: "1", Network: "solana-mainnet", Owner: wallet.PublicKey().String(), IntentID: intentID,
		ReservationID: reservationID, PolicySHA256: strings.Repeat("1", 64),
		SignedTransactionSHA256: transaction.SHA256, ProviderRequestID: "example-request",
		LastValidBlockHeight: "312345678", ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	req := signedApproval(t, key, approval)
	req.SignedTransaction = encoded
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := executeFunc(func(ctx context.Context, signed jupiter.SignedTransaction, requestID, height string) jupiter.ExecuteObservation {
		if calls.Add(1) != 1 || signed.Signature != transaction.Signature || requestID != approval.ProviderRequestID || height != approval.LastValidBlockHeight {
			t.Error("unexpected provider submission")
			return jupiter.ExecuteObservation{Status: "unknown"}
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return jupiter.ExecuteObservation{Status: "unknown", Code: new(-1001)}
	})
	preflightEntered, preflightRelease := make(chan struct{}, 2), make(chan struct{})
	rpc := &stubChain{preflight: func(ctx context.Context) error {
		select {
		case preflightEntered <- struct{}{}:
		case <-preflightRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-preflightRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	service := Service{owner: approval.Owner, policyHash: approval.PolicySHA256, key: pub, journal: store, provider: provider, chain: rpc}
	expired := approval
	expired.ExpiresAt = time.Now().UTC().Add(-time.Second)
	invalid := signedApproval(t, key, expired)
	invalid.SignedTransaction = encoded
	if _, err := service.Submit(ctx, invalid); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired approval reached submission: %v", err)
	}
	wrongPayload := approval
	wrongPayload.SignedTransactionSHA256 = strings.Repeat("3", 64)
	invalid = signedApproval(t, key, wrongPayload)
	invalid.SignedTransaction = encoded
	if _, err := service.Submit(ctx, invalid); !errors.Is(err, ErrUnauthorized) || calls.Load() != 0 {
		t.Fatalf("altered transaction binding reached submission: %v", err)
	}
	type outcome struct {
		receipt Receipt
		err     error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			got, err := service.Submit(ctx, req)
			results <- outcome{got, err}
		}()
	}
	for range 2 {
		select {
		case <-preflightEntered:
		case early := <-results:
			t.Fatalf("submit ended before concurrent claims: %+v", early)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(preflightRelease)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case duplicate := <-results:
		if duplicate.err != nil || duplicate.receipt.ProviderStatus != "unknown" ||
			duplicate.receipt.Signature != transaction.Signature || duplicate.receipt.ProviderCode != nil {
			t.Fatalf("durable claim not visible during send: %+v", duplicate)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	changed := approval
	changed.ProviderRequestID = "different-request"
	conflict := signedApproval(t, key, changed)
	conflict.SignedTransaction = encoded
	if _, err := service.Submit(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed approval was accepted: %v", err)
	}
	changed = approval
	changed.IntentID = "8264245a-4f43-4e3c-8ed7-9cb4f3ee3aca"
	conflict = signedApproval(t, key, changed)
	conflict.SignedTransaction = encoded
	if _, err := service.Submit(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("transaction claimed for a second intent: %v", err)
	}
	close(release)
	first := <-results
	if first.err != nil || first.receipt.ProviderStatus != "unknown" || first.receipt.ProviderCode == nil || *first.receipt.ProviderCode != -1001 {
		t.Fatalf("transport uncertainty was not retained: %+v", first)
	}
	reopened, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	service.journal = reopened
	replay, err := service.Submit(ctx, req)
	if err != nil || replay.ProviderStatus != "unknown" || replay.ProviderObservedAt == nil || calls.Load() != 1 {
		t.Fatalf("restart lost attempt or re-sent it: %+v / %v", replay, err)
	}
	receipt, err := service.Receipt(ctx, intentID)
	if err != nil || receipt.ChainStatus == nil || receipt.ChainStatus.State != "finalized_success" ||
		receipt.ProviderStatus != "unknown" || rpc.invalidations.Load() != 3 {
		t.Fatalf("chain observation did not reconcile uncertainty: %+v / %v", receipt, err)
	}
}

func TestPostgresRejectsMissingSignatureUniqueness(t *testing.T) {
	dsn := prepareTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("test PostgreSQL unavailable")
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "ALTER TABLE jupiter_execution_attempts DROP CONSTRAINT jupiter_execution_attempts_signature_key"); err != nil {
		t.Fatal(err)
	}
	store, err := OpenPostgres(ctx, dsn)
	if store != nil {
		store.Close()
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("journal accepted without unique signature constraint: %v", err)
	}
}

func prepareTestDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for PostgreSQL integration")
	}
	uri, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	schema := "jupiter_test_" + hex.EncodeToString(nonce[:])
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("test PostgreSQL unavailable")
	}
	migration, err := os.ReadFile("001_attempts.sql")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := db.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE")
		if err != nil {
			t.Error("failed to remove isolated test schema")
		}
		_ = db.Close(cleanup)
	})
	for _, statement := range []string{"CREATE SCHEMA " + schema, "SET search_path TO " + schema, string(migration)} {
		if _, err := db.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	query := uri.Query()
	query.Set("search_path", schema)
	uri.RawQuery = query.Encode()
	return uri.String()
}

func signedTestTransaction(t *testing.T, wallet *solana.Wallet) string {
	t.Helper()
	instruction := solana.NewInstruction(solana.SystemProgramID, nil, []byte{1, 2, 3, 4})
	tx, err := solana.NewTransaction([]solana.Instruction{instruction}, solana.Hash{1}, solana.TransactionPayer(wallet.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	tx.Message.SetVersion(solana.MessageVersionV0)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == wallet.PublicKey() {
			return &wallet.PrivateKey
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := tx.ToBase64()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type executeFunc func(context.Context, jupiter.SignedTransaction, string, string) jupiter.ExecuteObservation

func (execute executeFunc) Execute(ctx context.Context, signed jupiter.SignedTransaction, requestID, height string) jupiter.ExecuteObservation {
	return execute(ctx, signed, requestID, height)
}

type stubChain struct {
	invalidations atomic.Int32
	preflight     func(context.Context) error
}

func (chain *stubChain) CheckSubmission(ctx context.Context, _ string, _ uint64) error {
	return chain.preflight(ctx)
}

func (chain *stubChain) TransactionStatus(_ context.Context, signature string) (solana_balance.TransactionStatus, error) {
	return solana_balance.TransactionStatus{State: "finalized_success", Signature: signature}, nil
}

func (chain *stubChain) Invalidate() { chain.invalidations.Add(1) }
