package solana_balance

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testSignature = "4cdd1oX7cfVALfr26tP52BZ6cSzrgnNGtYD7BFhm6FFeZV5sPTnRvg6NRn8yC6DbEikXcrNChBM5vVJnTgKhGhVu"

func TestTransactionStatusObservesFinalityAndExactFees(t *testing.T) {
	for _, test := range []struct {
		name, status, transaction, state string
		wantErr                          bool
	}{
		{"unknown", `null`, "", "unknown", false},
		{"processed", `{"slot":44,"confirmationStatus":"processed","err":null}`, "", "processed", false},
		{"confirmed", `{"slot":44,"confirmationStatus":"confirmed","err":null}`, "", "confirmed", false},
		{"pruned transaction", `{"slot":44,"confirmationStatus":"finalized","err":null}`, `null`, "confirmed", false},
		{"success", `{"slot":44,"confirmationStatus":"finalized","err":null}`, transactionResult(testSignature, "null"), "finalized_success", false},
		{"failure", `{"slot":44,"confirmationStatus":"finalized","err":{"InstructionError":[1,"Custom"]}}`, transactionResult(testSignature, `{"InstructionError":[1,"Custom"]}`), "finalized_failure", false},
		{"wrong signature", `{"slot":44,"confirmationStatus":"finalized","err":null}`, transactionResult(strings.Repeat("1", 64), "null"), "", true},
		{"conflicting failure", `{"slot":44,"confirmationStatus":"finalized","err":null}`, transactionResult(testSignature, `"AccountInUse"`), "", true},
		{"absent error field", `{"slot":44,"confirmationStatus":"finalized"}`, "", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := transactionClient(t, func(method string, params []json.RawMessage) string {
				switch method {
				case "getSignatureStatuses":
					if len(params) != 2 || string(params[0]) != `["`+testSignature+`"]` || string(params[1]) != `{"searchTransactionHistory":true}` {
						t.Errorf("unexpected status params %s", params)
					}
					return `{"context":{"slot":45},"value":[` + test.status + `]}`
				case "getTransaction":
					if len(params) != 2 || string(params[0]) != `"`+testSignature+`"` || test.transaction == "" {
						t.Errorf("unexpected request for transaction %s", params)
					} else {
						var options map[string]any
						if json.Unmarshal(params[1], &options) != nil || options["commitment"] != "finalized" || options["encoding"] != "json" || options["maxSupportedTransactionVersion"] != float64(0) {
							t.Errorf("unexpected getTransaction configuration %s", params[1])
						}
					}
					return test.transaction
				default:
					t.Errorf("unexpected method %s", method)
					return `null`
				}
			})
			got, err := client.TransactionStatus(t.Context(), testSignature)
			if test.wantErr {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("expected unavailable on invalid data, got %+v, %v", got, err)
				}
				return
			}
			if err != nil || got.SchemaVersion != "1" || got.Network != "solana-mainnet" || got.Signature != testSignature || got.State != test.state || got.ContextSlot != 45 || got.ObservedAt.IsZero() {
				t.Fatalf("unexpected observation %+v, %v", got, err)
			}
			if test.state == "unknown" && got.TransactionSlot != nil || test.state != "unknown" && (got.TransactionSlot == nil || *got.TransactionSlot != 44) {
				t.Fatalf("unexpected transaction slot %+v", got)
			}
			if strings.HasPrefix(test.state, "finalized_") {
				if got.FeeLamports == nil || *got.FeeLamports != maxAmount {
					t.Fatalf("fee is missing or imprecise %+v", got)
				}
			} else if got.FeeLamports != nil {
				t.Fatalf("unexpected fee on a non-finalized observation %+v", got)
			}
		})
	}
}

func TestTransactionStatusRejectsMalformedSignatureWithoutRPC(t *testing.T) {
	client := fixtureClient(t, 0, func(http.ResponseWriter, *http.Request) { t.Error("malformed signature reached RPC") })
	for _, signature := range []string{"", strings.Repeat("1", 63), strings.Repeat("z", 88)} {
		if _, err := client.TransactionStatus(t.Context(), signature); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("unexpected error %v", err)
		}
	}
}

func TestCheckSubmissionVerifiesConfirmedHeightAndBlockhash(t *testing.T) {
	for _, test := range []struct {
		name, height, validity string
		wantErr                error
	}{
		{"at last valid height", "100", "true", nil},
		{"expired by height", "101", "true", ErrExpired},
		{"invalid blockhash", "99", "false", ErrExpired},
		{"missing validity", "99", "null", ErrUnavailable},
		{"missing height", "null", "true", ErrUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := transactionClient(t, func(method string, params []json.RawMessage) string {
				switch method {
				case "getBlockHeight":
					if len(params) != 1 || string(params[0]) != `{"commitment":"confirmed"}` {
						t.Errorf("unexpected height params %s", params)
					}
					return test.height
				case "isBlockhashValid":
					if len(params) != 2 || string(params[0]) != `"`+testOwner+`"` || string(params[1]) != `{"commitment":"confirmed"}` {
						t.Errorf("unexpected validity params %s", params)
					}
					return `{"context":{"slot":110},"value":` + test.validity + `}`
				default:
					t.Errorf("unexpected method %s", method)
					return `null`
				}
			})
			if err := client.CheckSubmission(t.Context(), testOwner, 100); !errors.Is(err, test.wantErr) {
				t.Fatalf("submission check = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestInvalidateDetachesPendingBalanceRefresh(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	client := fixtureClient(t, time.Minute, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 4 {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		writeFixture(t, w, r, "")
	})
	done := make(chan error, 1)
	go func() {
		_, err := client.Balances(t.Context(), false)
		done <- err
	}()
	for range 4 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("ordinary refresh did not reach the RPC")
		}
	}
	client.Invalidate()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := client.Balances(t.Context(), false); err != nil || calls.Load() != 8 {
		t.Fatalf("invalidated request repopulated cache: %d RPC calls, %v", calls.Load(), err)
	}
}

func transactionResult(signature, transactionError string) string {
	return fmt.Sprintf(`{"slot":44,"version":0,"transaction":{"signatures":[%q]},"meta":{"err":%s,"fee":%s}}`, signature, transactionError, maxAmount)
}

func transactionClient(t *testing.T, result func(string, []json.RawMessage) string) *Client {
	t.Helper()
	return fixtureClient(t, 0, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("read RPC request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		raw := `"` + mainnetGenesisHash + `"`
		if request.Method != "getGenesisHash" {
			raw = result(request.Method, request.Params)
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%s}`, raw)
	})
}
