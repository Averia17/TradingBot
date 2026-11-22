package jupiter_execution

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"filippo.io/edwards25519"

	"tradingbot/venue-quotes/internal/quotes"
)

const approvalDomain = "TradingBot Jupiter Swap Approval v1\n"

var (
	ErrInvalid      = errors.New("invalid Jupiter execution request")
	ErrUnauthorized = errors.New("Jupiter execution approval rejected")
	ErrConflict     = errors.New("Jupiter execution attempt conflicts with existing attempt")
	ErrExpired      = errors.New("Jupiter execution approval or transaction expired")
	ErrNotFound     = errors.New("Jupiter execution attempt not found")
	ErrUnavailable  = errors.New("Jupiter execution service unavailable")

	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hashPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
)

type Request struct {
	Approval          string `json:"approval"`
	ApprovalSignature string `json:"approval_signature"`
	SignedTransaction string `json:"signed_transaction_base64"`
}

// Approval is the independent verifier's authorization of an exact signed swap
// against an active owner policy and a reservation issued by the account writer.
type Approval struct {
	SchemaVersion           string    `json:"schema_version"`
	Network                 string    `json:"network"`
	Owner                   string    `json:"owner"`
	IntentID                string    `json:"intent_id"`
	ReservationID           string    `json:"reservation_id"`
	PolicySHA256            string    `json:"policy_sha256"`
	SignedTransactionSHA256 string    `json:"signed_transaction_sha256"`
	ProviderRequestID       string    `json:"provider_request_id"`
	LastValidBlockHeight    string    `json:"last_valid_block_height"`
	ExpiresAt               time.Time `json:"expires_at"`
}

// ApprovalMessage describes the bytes signed by the independent approval key.
func ApprovalMessage(canonicalJSON []byte) []byte {
	return append([]byte(approvalDomain), canonicalJSON...)
}

func validateApproval(req Request, publicKey ed25519.PublicKey, owner, policyHash string) (Approval, string, error) {
	raw, valid := decodeBase64(req.Approval, 2048)
	signature, validSignature := decodeBase64(req.ApprovalSignature, ed25519.SignatureSize)
	if !valid || !validSignature || len(signature) != ed25519.SignatureSize {
		return Approval{}, "", ErrInvalid
	}
	message := ApprovalMessage(raw)
	if !ed25519.Verify(publicKey, message, signature) {
		return Approval{}, "", ErrUnauthorized
	}
	var a Approval
	if json.Unmarshal(raw, &a) != nil {
		return Approval{}, "", ErrInvalid
	}
	canonical, err := json.Marshal(a)
	if err != nil || !bytes.Equal(raw, canonical) || a.SchemaVersion != "1" || a.Network != quotes.SolanaMainnet ||
		!validID(a.IntentID) || !validID(a.ReservationID) || !hashPattern.MatchString(a.SignedTransactionSHA256) ||
		!requestIDPattern.MatchString(a.ProviderRequestID) || a.ExpiresAt.IsZero() {
		return Approval{}, "", ErrInvalid
	}
	if _, valid := quotes.PositiveAmount(a.LastValidBlockHeight); !valid {
		return Approval{}, "", ErrInvalid
	}
	if a.Owner != owner || a.PolicySHA256 != policyHash {
		return Approval{}, "", ErrUnauthorized
	}
	sum := sha256.Sum256(message)
	return a, hex.EncodeToString(sum[:]), nil
}

func DecodeApprovalPublicKey(encoded string) (ed25519.PublicKey, error) {
	key, valid := decodeBase64(encoded, ed25519.PublicKeySize)
	if !valid || !validApprovalKey(key) {
		return nil, ErrInvalid
	}
	return ed25519.PublicKey(key), nil
}

func validApprovalKey(key ed25519.PublicKey) bool {
	point, err := new(edwards25519.Point).SetBytes(key)
	return err == nil && bytes.Equal(point.Bytes(), key) &&
		new(edwards25519.Point).MultByCofactor(point).Equal(edwards25519.NewIdentityPoint()) != 1
}

func decodeBase64(encoded string, maxBytes int) ([]byte, bool) {
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(maxBytes) {
		return nil, false
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	return decoded, err == nil && len(decoded) <= maxBytes && base64.StdEncoding.EncodeToString(decoded) == encoded
}

func validID(s string) bool {
	return s != "00000000-0000-0000-0000-000000000000" && uuidPattern.MatchString(s)
}
