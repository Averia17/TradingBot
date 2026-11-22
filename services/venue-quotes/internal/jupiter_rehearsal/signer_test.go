package jupiter_rehearsal

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	solana "github.com/solana-foundation/solana-go/v2"

	"tradingbot/venue-quotes/internal/jupiter"
	"tradingbot/venue-quotes/internal/jupiter_execution"
	"tradingbot/venue-quotes/internal/quotes"
)

const (
	fixtureAssetMint            = "BQ72nSv9f3PRyRKCBnHLVrerrv37CYTHm5h3s9VSGQDV"
	fixtureUSDC                 = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	fixtureLastValidBlockHeight = "312345678"
	fixtureTradeDomain          = "TradingBot Jupiter Offline Trade v1\n"
)

var (
	fixtureBlockhash   = solana.Hash(sha256.Sum256([]byte("TradingBot Jupiter Offline Blockhash v1")))
	fixtureUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	errFixtureApproval = errors.New("offline Jupiter fixture does not satisfy signer policy")
)

type fixtureSigner struct {
	wallet      *solana.Wallet
	approvalKey ed25519.PublicKey
	approvalSK  ed25519.PrivateKey
	policyHash  string
}

func newSigner(t *testing.T) *fixtureSigner {
	t.Helper()
	wallet := solana.NewWallet()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal("could not create offline approval key")
	}
	policy := "TradingBot Jupiter Offline Fixture Policy v1\n" + wallet.PublicKey().String() + "\n" + fixtureAssetMint + "\n" + fixtureBlockhash.String()
	hash := sha256.Sum256([]byte(policy))
	return &fixtureSigner{wallet: wallet, approvalKey: publicKey, approvalSK: privateKey, policyHash: hex.EncodeToString(hash[:])}
}

func (signer *fixtureSigner) owner() string {
	return signer.wallet.PublicKey().String()
}

type fixtureTrade struct {
	Owner       string `json:"owner"`
	CaseID      string `json:"case_id"`
	Side        string `json:"side"`
	InputMint   string `json:"input_mint"`
	OutputMint  string `json:"output_mint"`
	InAmount    string `json:"in_amount"`
	OutAmount   string `json:"out_amount"`
	MinOut      string `json:"minimum_output"`
	SlippageBPS uint16 `json:"slippage_bps"`
}

func fixtureTransaction(owner string, request jupiter.SwapRequest, caseID string) (*solana.Transaction, fixtureTrade, error) {
	if request.AssetMint != fixtureAssetMint || request.SlippageBPS != 1000 {
		return nil, fixtureTrade{}, errFixtureApproval
	}
	terms := fixtureTrade{Owner: owner, CaseID: caseID, Side: request.Side, InAmount: request.InAmount, MinOut: request.MinOutAmount, SlippageBPS: request.SlippageBPS}
	switch {
	case request.Side == "BUY" && caseID == "fixture-buy" && request.InAmount == "200000000" && request.MinOutAmount == "100":
		terms.InputMint, terms.OutputMint, terms.OutAmount = fixtureUSDC, fixtureAssetMint, "110"
	case request.Side == "SELL" && caseID == "fixture-sell" && request.InAmount == "100" && request.MinOutAmount == "210000000":
		terms.InputMint, terms.OutputMint, terms.OutAmount = fixtureAssetMint, fixtureUSDC, "220000000"
	default:
		return nil, fixtureTrade{}, errFixtureApproval
	}
	ownerKey, err := solana.PublicKeyFromBase58(owner)
	if err != nil || ownerKey.IsZero() {
		return nil, fixtureTrade{}, errFixtureApproval
	}
	memo, err := json.Marshal(terms)
	if err != nil {
		return nil, fixtureTrade{}, errFixtureApproval
	}
	instruction := solana.NewInstruction(solana.MemoProgramID, nil, append([]byte(fixtureTradeDomain), memo...))
	tx, err := solana.NewTransaction([]solana.Instruction{instruction}, fixtureBlockhash, solana.TransactionPayer(ownerKey))
	if err != nil {
		return nil, fixtureTrade{}, errFixtureApproval
	}
	tx.Message.SetVersion(solana.MessageVersionV0)
	tx.Signatures = []solana.Signature{{}}
	if tx.Sanitize() != nil {
		return nil, fixtureTrade{}, errFixtureApproval
	}
	return tx, terms, nil
}

// buildCandidate is the public part of the fixture. The fake venue uses it
// without either signing key.
func buildCandidate(owner string, request jupiter.SwapRequest, caseID string) (string, error) {
	tx, _, err := fixtureTransaction(owner, request, caseID)
	if err != nil {
		return "", err
	}
	return tx.ToBase64()
}

func (signer *fixtureSigner) approve(prepared jupiter.PreparedSwap, expected jupiter.SwapRequest, intentID, reservationID string, expiresAt time.Time) (jupiter_execution.Request, error) {
	now := time.Now().UTC()
	if !validFixtureID(intentID) || !validFixtureID(reservationID) || !expiresAt.After(now) ||
		(prepared.ProviderExpiresAt != nil && expiresAt.After(*prepared.ProviderExpiresAt)) {
		return jupiter_execution.Request{}, errFixtureApproval
	}
	tx, terms, err := fixtureTransaction(signer.owner(), expected, prepared.ProviderRequestID)
	if err != nil {
		return jupiter_execution.Request{}, err
	}
	canonical, err := tx.ToBase64()
	if err != nil || canonical != prepared.TransactionBase64 ||
		prepared.SchemaVersion != "1" || prepared.State != "prepared" || prepared.Network != quotes.SolanaMainnet ||
		prepared.Router != "metis" || prepared.TransactionVersion != 0 || prepared.LastValidBlockHeight != fixtureLastValidBlockHeight ||
		prepared.Owner != terms.Owner || prepared.Side != terms.Side || prepared.AssetMint != expected.AssetMint ||
		prepared.InputMint != terms.InputMint || prepared.OutputMint != terms.OutputMint || prepared.InAmount != terms.InAmount ||
		prepared.OutAmount != terms.OutAmount || prepared.MinOutAmount != terms.MinOut || prepared.RequestedMinOutAmount != terms.MinOut ||
		prepared.SlippageBPS != terms.SlippageBPS || prepared.SwapFeeBPS != nil || prepared.SwapFeeMint != nil ||
		prepared.SignatureFeeLamports != "5000" || prepared.PriorityFeeLamports != "0" || prepared.RentFeeLamports != "0" ||
		prepared.RequestedAt.IsZero() || prepared.ObservedAt.Before(prepared.RequestedAt) || prepared.ObservedAt.After(now) {
		return jupiter_execution.Request{}, errFixtureApproval
	}

	// Sign the independently rebuilt Memo, never arbitrary provider bytes.
	if _, err := tx.Sign(func(pubkey solana.PublicKey) *solana.PrivateKey {
		if pubkey != signer.wallet.PublicKey() {
			return nil
		}
		return &signer.wallet.PrivateKey
	}); err != nil {
		return jupiter_execution.Request{}, errFixtureApproval
	}
	encoded, err := tx.ToBase64()
	if err != nil {
		return jupiter_execution.Request{}, errFixtureApproval
	}
	signed, err := jupiter.ValidateSignedSwap(encoded, signer.owner())
	if err != nil {
		return jupiter_execution.Request{}, errFixtureApproval
	}
	approval, err := json.Marshal(jupiter_execution.Approval{
		SchemaVersion: "1", Network: quotes.SolanaMainnet, Owner: signer.owner(), IntentID: intentID, ReservationID: reservationID,
		PolicySHA256: signer.policyHash, SignedTransactionSHA256: signed.SHA256, ProviderRequestID: terms.CaseID,
		LastValidBlockHeight: fixtureLastValidBlockHeight, ExpiresAt: expiresAt.UTC(),
	})
	if err != nil {
		return jupiter_execution.Request{}, errFixtureApproval
	}
	signature := ed25519.Sign(signer.approvalSK, jupiter_execution.ApprovalMessage(approval))
	return jupiter_execution.Request{
		Approval: base64.StdEncoding.EncodeToString(approval), ApprovalSignature: base64.StdEncoding.EncodeToString(signature), SignedTransaction: encoded,
	}, nil
}

func validFixtureID(id string) bool {
	return id != "00000000-0000-0000-0000-000000000000" && fixtureUUIDPattern.MatchString(id)
}

func TestOfflineSignerApprovesOnlyTheIndependentFixture(t *testing.T) {
	signer := newSigner(t)
	expected := jupiter.SwapRequest{Side: "BUY", AssetMint: fixtureAssetMint, InAmount: "200000000", MinOutAmount: "100", SlippageBPS: 1000}
	const intentID = "c48acacc-476a-445d-ad3c-c8cd157519e3"
	const reservationID = "df9b434e-de61-4b39-aae6-8848c582702f"
	expiresAt := time.Now().UTC().Add(time.Minute)
	prepared := signerTestCandidate(t, signer.owner(), expected)
	request, err := signer.approve(prepared, expected, intentID, reservationID, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := jupiter.ValidateSignedSwap(request.SignedTransaction, signer.owner())
	if err != nil || signed.RecentBlockhash != fixtureBlockhash.String() {
		t.Fatalf("invalid signed fixture: %v", err)
	}
	rawApproval, err := base64.StdEncoding.DecodeString(request.Approval)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.StdEncoding.DecodeString(request.ApprovalSignature)
	if err != nil || !ed25519.Verify(signer.approvalKey, jupiter_execution.ApprovalMessage(rawApproval), signature) {
		t.Fatal("approval signature did not validate")
	}
	var approval jupiter_execution.Approval
	if err := json.Unmarshal(rawApproval, &approval); err != nil || approval.PolicySHA256 != signer.policyHash ||
		approval.SignedTransactionSHA256 != signed.SHA256 || approval.IntentID != intentID || approval.ReservationID != reservationID ||
		approval.ProviderRequestID != "fixture-buy" || !approval.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("approval did not bind the fixture: %v", err)
	}

	for _, test := range []struct {
		name   string
		change func(*jupiter.PreparedSwap)
	}{
		{"different owner", func(candidate *jupiter.PreparedSwap) { candidate.Owner = solana.NewWallet().PublicKey().String() }},
		{"different request ID", func(candidate *jupiter.PreparedSwap) { candidate.ProviderRequestID = "fixture-sell" }},
		{"different minimum", func(candidate *jupiter.PreparedSwap) { candidate.MinOutAmount = "99" }},
		{"different output", func(candidate *jupiter.PreparedSwap) { candidate.OutAmount = "111" }},
		{"different fee", func(candidate *jupiter.PreparedSwap) { candidate.PriorityFeeLamports = "1" }},
		{"different height", func(candidate *jupiter.PreparedSwap) { candidate.LastValidBlockHeight = "312345679" }},
		{"trailing wire bytes", func(candidate *jupiter.PreparedSwap) {
			raw, err := base64.StdEncoding.DecodeString(candidate.TransactionBase64)
			if err != nil {
				t.Fatal(err)
			}
			candidate.TransactionBase64 = base64.StdEncoding.EncodeToString(append(raw, 0))
		}},
		{"different payer in wire", func(candidate *jupiter.PreparedSwap) {
			candidate.TransactionBase64 = signerTestCandidate(t, solana.NewWallet().PublicKey().String(), expected).TransactionBase64
		}},
		{"tampered instruction data", func(candidate *jupiter.PreparedSwap) {
			mutateFixtureWire(t, candidate, func(tx *solana.Transaction) { tx.Message.Instructions[0].Data[0] ^= 1 })
		}},
		{"extra instruction", func(candidate *jupiter.PreparedSwap) {
			mutateFixtureWire(t, candidate, func(tx *solana.Transaction) {
				tx.Message.Instructions = append(tx.Message.Instructions, tx.Message.Instructions[0])
			})
		}},
		{"unexpected prior signature", func(candidate *jupiter.PreparedSwap) {
			mutateFixtureWire(t, candidate, func(tx *solana.Transaction) { tx.Signatures[0][0] = 1 })
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := prepared
			test.change(&candidate)
			if result, err := signer.approve(candidate, expected, intentID, reservationID, expiresAt); err == nil || result != (jupiter_execution.Request{}) {
				t.Fatal("fixture verifier approved altered data")
			}
		})
	}
	for _, test := range []struct {
		intentID      string
		reservationID string
		expiresAt     time.Time
	}{
		{"invalid-intent", reservationID, expiresAt},
		{intentID, "00000000-0000-0000-0000-000000000000", expiresAt},
		{intentID, reservationID, time.Now().UTC().Add(-time.Second)},
	} {
		if _, err := signer.approve(prepared, expected, test.intentID, test.reservationID, test.expiresAt); err == nil {
			t.Fatal("fixture verifier accepted invalid approval metadata")
		}
	}
}

func signerTestCandidate(t *testing.T, owner string, request jupiter.SwapRequest) jupiter.PreparedSwap {
	t.Helper()
	encoded, err := buildCandidate(owner, request, "fixture-buy")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return jupiter.PreparedSwap{
		SchemaVersion: "1", State: "prepared", Network: quotes.SolanaMainnet, Router: "metis", Owner: owner, Side: request.Side, AssetMint: fixtureAssetMint,
		InputMint: fixtureUSDC, OutputMint: fixtureAssetMint, InAmount: request.InAmount, OutAmount: "110", MinOutAmount: request.MinOutAmount,
		RequestedMinOutAmount: request.MinOutAmount, SlippageBPS: 1000, SignatureFeeLamports: "5000", PriorityFeeLamports: "0", RentFeeLamports: "0",
		ProviderRequestID: "fixture-buy", LastValidBlockHeight: fixtureLastValidBlockHeight, TransactionBase64: encoded, RequestedAt: now, ObservedAt: now,
	}
}

func mutateFixtureWire(t *testing.T, candidate *jupiter.PreparedSwap, change func(*solana.Transaction)) {
	t.Helper()
	tx, err := solana.TransactionFromBase64(candidate.TransactionBase64)
	if err != nil {
		t.Fatal(err)
	}
	change(tx)
	candidate.TransactionBase64, err = tx.ToBase64()
	if err != nil {
		t.Fatal(err)
	}
}
