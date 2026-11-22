package jupiter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	solana "github.com/solana-foundation/solana-go/v2"
)

func TestValidateSignedSwapRejectsUnauthenticatedOrUnsupportedBytes(t *testing.T) {
	wallet := solana.NewWallet()
	owner := wallet.PublicKey().String()
	valid := fixtureSignedTransaction(t, wallet, solana.MessageVersionV0, 2)
	verified, err := ValidateSignedSwap(valid, owner)
	if err != nil || verified.Signature == "" || verified.RecentBlockhash == "" || len(verified.SHA256) != 64 {
		t.Fatalf("valid signed transaction rejected: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(valid)
	extra := base64.StdEncoding.EncodeToString(append(append([]byte{}, raw...), 0))
	raw[5] ^= 1
	mutated := base64.StdEncoding.EncodeToString(raw)
	for _, test := range []struct{ name, encoded, owner string }{
		{"wrong fee payer", valid, solana.NewWallet().PublicKey().String()},
		{"trailing bytes", extra, owner},
		{"mutated signature", mutated, owner},
		{"legacy message", fixtureSignedTransaction(t, wallet, solana.MessageVersionLegacy, 2), owner},
		{"durable nonce", fixtureSignedTransaction(t, wallet, solana.MessageVersionV0, 4), owner},
		{"invalid encoding", "abc!", owner},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ValidateSignedSwap(test.encoded, test.owner); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("invalid transaction accepted: %v", err)
			}
		})
	}
}

func TestExecuteCallsJupiterOnceAndTreatsOutcomeAsObservation(t *testing.T) {
	wallet := solana.NewWallet()
	encoded := fixtureSignedTransaction(t, wallet, solana.MessageVersionV0, 2)
	tx, err := ValidateSignedSwap(encoded, wallet.PublicKey().String())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, body, state string
		statusCode        int
		requestError      error
	}{
		{"accepted", `{"status":"Success","code":0,"signature":"` + tx.Signature + `","slot":"9007199254740993"}`, "accepted", 200, nil},
		{"reported rejection", `{"status":"Failed","code":-1005}`, "rejected", 200, nil},
		{"upstream unknown", `{"status":"Failed","code":-1001}`, "unknown", 200, nil},
		{"missing success slot", `{"status":"Success","code":0,"signature":"` + tx.Signature + `"}`, "unknown", 200, nil},
		{"wrong signature", `{"status":"Success","code":0,"signature":"other","slot":"1"}`, "unknown", 200, nil},
		{"malformed", "not-json", "unknown", 200, nil},
		{"server error", "", "unknown", 500, nil},
		{"redirect", "", "unknown", 307, nil},
		{"timeout", "", "unknown", 0, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := New("test-key")
			calls := 0
			client.http.Transport = transportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Method != http.MethodPost || request.URL.String() != "https://api.jup.ag/swap/v2/execute" ||
					request.Header.Get("x-api-key") != "test-key" || request.Header.Get("Content-Type") != "application/json" ||
					request.GetBody != nil || request.Header.Get("Idempotency-Key") != "" {
					t.Fatalf("unexpected Jupiter execute request: %s %s", request.Method, request.URL)
				}
				defer request.Body.Close()
				var body map[string]string
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil || !reflect.DeepEqual(body, map[string]string{
					"signedTransaction": encoded, "requestId": "order-1", "lastValidBlockHeight": "12345",
				}) {
					t.Fatal("incorrect Jupiter request body")
				}
				if test.requestError != nil {
					return nil, test.requestError
				}
				return &http.Response{StatusCode: test.statusCode, Header: http.Header{"Location": {"https://other.invalid"}},
					Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})
			got := client.Execute(t.Context(), tx, "order-1", "12345")
			if calls != 1 || got.Status != test.state {
				t.Fatalf("calls=%d observation=%+v", calls, got)
			}
			if got.Status == "accepted" && (got.Code == nil || *got.Code != 0 || got.Slot == nil || *got.Slot != 9007199254740993) {
				t.Fatalf("accepted without matching exact slot: %+v", got)
			}
		})
	}
}

func fixtureSignedTransaction(t *testing.T, wallet *solana.Wallet, version solana.MessageVersion, instruction byte) string {
	t.Helper()
	tx, err := solana.NewTransaction([]solana.Instruction{solana.NewInstruction(solana.SystemProgramID, nil,
		[]byte{instruction, 0, 0, 0})}, solana.Hash{1}, solana.TransactionPayer(wallet.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Message.SetVersion(version); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == wallet.PublicKey() {
			return &wallet.PrivateKey
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	encoded, err := tx.ToBase64()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
