package jupiter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"

	solana "github.com/solana-foundation/solana-go/v2"

	"tradingbot/venue-quotes/internal/quotes"
)

const executePath = "/swap/v2/execute"

type SignedTransaction struct {
	Signature       string
	RecentBlockhash string
	SHA256          string
	encoded         string
}

type ExecuteObservation struct {
	Status string
	Code   *int
	Slot   *uint64
}

// ValidateSignedSwap checks the exact wire bytes and the configured wallet
// signature. Economic authorization comes from the independent verifier.
func ValidateSignedSwap(encoded, owner string) (SignedTransaction, error) {
	if len(encoded) > base64.StdEncoding.EncodedLen(maxV0TransactionBytes) {
		return SignedTransaction{}, ErrInvalidRequest
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw) > maxV0TransactionBytes || base64.StdEncoding.EncodeToString(raw) != encoded {
		return SignedTransaction{}, ErrInvalidRequest
	}
	ownerKey, err := solana.PublicKeyFromBase58(owner)
	if err != nil {
		return SignedTransaction{}, ErrInvalidRequest
	}
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil || tx.Message.GetVersion() != solana.MessageVersionV0 || len(tx.Signatures) != 1 ||
		tx.Sanitize() != nil || tx.Message.AccountKeys[0] != ownerKey || tx.Message.RecentBlockhash == (solana.Hash{}) ||
		tx.Signatures[0] == (solana.Signature{}) || tx.UsesDurableNonce() || tx.VerifySignatures() != nil {
		return SignedTransaction{}, ErrInvalidRequest
	}
	canonical, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(raw, canonical) {
		return SignedTransaction{}, ErrInvalidRequest
	}
	hash := sha256.Sum256(raw)
	return SignedTransaction{
		Signature: tx.Signatures[0].String(), RecentBlockhash: tx.Message.RecentBlockhash.String(),
		SHA256: hex.EncodeToString(hash[:]), encoded: encoded,
	}, nil
}

type executeResponse struct {
	Status    string `json:"status"`
	Signature string `json:"signature"`
	Slot      string `json:"slot"`
	Code      *int32 `json:"code"`
	Error     string `json:"error"`
}

// Execute submits once. It returns an observation of Jupiter's response, not
// proof of finality. Ambiguous outcomes must be looked up by local signature.
func (client *Client) Execute(ctx context.Context, tx SignedTransaction, providerRequestID, lastValidBlockHeight string) ExecuteObservation {
	unknown := ExecuteObservation{Status: "unknown"}
	if client.apiKey == "" || tx.encoded == "" || providerRequestID == "" {
		return unknown
	}
	if _, valid := quotes.PositiveAmount(lastValidBlockHeight); !valid {
		return unknown
	}
	body, err := json.Marshal(struct {
		SignedTransaction    string `json:"signedTransaction"`
		RequestID            string `json:"requestId"`
		LastValidBlockHeight string `json:"lastValidBlockHeight"`
	}{tx.encoded, providerRequestID, lastValidBlockHeight})
	if err != nil {
		return unknown
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiOrigin+executePath, bytes.NewReader(body))
	if err != nil {
		return unknown
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", client.apiKey)
	resp, err := client.http.Do(req)
	if err != nil {
		return unknown
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return unknown
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	var result executeResponse
	if err != nil || len(raw) > maxResponseBytes || !utf8.Valid(raw) || json.Unmarshal(raw, &result) != nil ||
		result.Code == nil || (result.Signature != "" && result.Signature != tx.Signature) {
		return unknown
	}
	observation := ExecuteObservation{Status: "unknown", Code: new(int(*result.Code))}
	if result.Slot != "" {
		slot, valid := quotes.PositiveAmount(result.Slot)
		if !valid {
			return unknown
		}
		observation.Slot = &slot
	}
	switch {
	case result.Status == "Success" && *result.Code == 0 && result.Signature == tx.Signature && observation.Slot != nil && result.Error == "":
		observation.Status = "accepted"
	case result.Status == "Failed" && *result.Code != 0 && *result.Code != -1001:
		observation.Status = "rejected"
	default:
		observation.Slot = nil
	}
	return observation
}
