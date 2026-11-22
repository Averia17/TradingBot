package jupiter_execution

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const owner = "BQ72nSv9f3PRyRKCBnHLVrerrv37CYTHm5h3s9VSGQDV"
const intentID = "7c4d4963-94e7-498b-8869-4e46feacbdd0"
const reservationID = "7c4d4963-94e7-498b-8869-4e46feacbdd1"

func TestApprovalBindsTheExactBytesAndPolicy(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approval := Approval{
		SchemaVersion: "1", Network: "solana-mainnet", Owner: owner, IntentID: intentID,
		ReservationID: reservationID, PolicySHA256: strings.Repeat("1", 64),
		SignedTransactionSHA256: strings.Repeat("2", 64), ProviderRequestID: "request-1",
		LastValidBlockHeight: "312345678", ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	req := signedApproval(t, key, approval)
	got, hash, err := validateApproval(req, pub, owner, approval.PolicySHA256)
	if err != nil || got != approval || len(hash) != 64 {
		t.Fatalf("approval=%+v hash=%q error=%v", got, hash, err)
	}
	if _, _, err := validateApproval(req, pub, owner, strings.Repeat("3", 64)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unapproved owner policy: %v", err)
	}
	text, _ := base64.StdEncoding.DecodeString(req.Approval)
	req.Approval = base64.StdEncoding.EncodeToString(append(text, ' '))
	if _, _, err := validateApproval(req, pub, owner, approval.PolicySHA256); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("altered bytes: %v", err)
	}

	// A trusted key still cannot authorize a document with ambiguous fields.
	raw := append(append([]byte{}, text[:len(text)-1]...), []byte(`,"intent_id":"`+intentID+`"}`)...)
	req.Approval = base64.StdEncoding.EncodeToString(raw)
	req.ApprovalSignature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, ApprovalMessage(raw)))
	if _, _, err := validateApproval(req, pub, owner, approval.PolicySHA256); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ambiguous signed approval: %v", err)
	}
}

func TestApprovalPublicKeyRejectsSmallOrderPoint(t *testing.T) {
	identity := make([]byte, ed25519.PublicKeySize)
	identity[0] = 1
	if _, err := DecodeApprovalPublicKey(base64.StdEncoding.EncodeToString(identity)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted identity public key: %v", err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeApprovalPublicKey(base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatalf("rejected generated public key: %v", err)
	}
}

func signedApproval(t *testing.T, key ed25519.PrivateKey, approval Approval) Request {
	t.Helper()
	raw, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	return Request{
		Approval:          base64.StdEncoding.EncodeToString(raw),
		ApprovalSignature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, ApprovalMessage(raw))),
	}
}
