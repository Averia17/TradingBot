package jupiter_rehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"testing"

	solana "github.com/solana-foundation/solana-go/v2"

	"tradingbot/venue-quotes/internal/jupiter"
)

const (
	fixtureRPCURL   = "http://127.0.0.1:1/"
	fixtureAPIKey   = "rehearsal-jupiter-api-key"
	fixtureGenesis  = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	fixtureSPL      = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	fixtureToken22  = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	fixtureBaseSlot = 320000000
	fixtureFee      = 5000
)

type venueOrder struct {
	id, side                     string
	inputMint, outputMint        string
	input, quoted, floor, filled uint64
}

var venueOrders = [...]venueOrder{
	{id: "fixture-buy", side: "BUY", inputMint: fixtureUSDC, outputMint: fixtureAssetMint, input: 200000000, quoted: 110, floor: 100, filled: 100},
	{id: "fixture-sell", side: "SELL", inputMint: fixtureAssetMint, outputMint: fixtureUSDC, input: 100, quoted: 220000000, floor: 210000000, filled: 215000000},
}

type venueAttempt struct {
	order     venueOrder
	slot      uint64
	finalized bool
}

// fixtureVenue answers the exact upstream protocols in process and never dials.
type fixtureVenue struct {
	mu                            sync.Mutex
	owner                         string
	usdc, asset, lamports, slot   uint64
	messages                      map[string][]byte
	attempts                      map[string]venueAttempt
	executeCalls, reads, rejected int
}

func newVenue(owner string) *fixtureVenue {
	return &fixtureVenue{
		owner: owner, usdc: 1000000000, lamports: 20000000, slot: fixtureBaseSlot,
		messages: make(map[string][]byte), attempts: make(map[string]venueAttempt),
	}
}

func (venue *fixtureVenue) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		defer r.Body.Close()
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	venue.mu.Lock()
	defer venue.mu.Unlock()
	if r.URL.User != nil || r.URL.Fragment != "" || r.URL.Opaque != "" || r.URL.RawPath != "" {
		return venue.reject()
	}
	if r.URL.String() == fixtureRPCURL && r.Method == http.MethodPost && r.Header.Get("Content-Type") == "application/json" {
		return venue.solanaRPC(r)
	}
	if r.URL.Scheme != "https" || r.URL.Host != "api.jup.ag" || r.Header.Get("x-api-key") != fixtureAPIKey {
		return venue.reject()
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/price/v3":
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || !reflect.DeepEqual(query, url.Values{"ids": {fixtureAssetMint}}) {
			return venue.reject()
		}
		return fixtureResponse(r, map[string]any{
			fixtureAssetMint: map[string]any{"usdPrice": json.Number("2.10"), "decimals": 0, "blockId": venue.slot},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/swap/v2/order":
		return venue.order(r)
	case r.Method == http.MethodPost && r.URL.Path == "/swap/v2/execute" && r.URL.RawQuery == "" &&
		r.Header.Get("Content-Type") == "application/json":
		return venue.execute(r)
	default:
		return venue.reject()
	}
}

func (venue *fixtureVenue) order(r *http.Request) (*http.Response, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return venue.reject()
	}
	for _, order := range venueOrders {
		if query.Get("inputMint") != order.inputMint || query.Get("outputMint") != order.outputMint ||
			query.Get("amount") != rawAmount(order.input) {
			continue
		}
		expected := url.Values{"inputMint": {order.inputMint}, "outputMint": {order.outputMint}, "amount": {rawAmount(order.input)}}
		response := map[string]any{
			"inputMint": order.inputMint, "outputMint": order.outputMint, "inAmount": rawAmount(order.input),
			"outAmount": rawAmount(order.quoted), "otherAmountThreshold": rawAmount(order.floor),
			"swapMode": "ExactIn", "router": "metis", "requestId": order.id, "transaction": nil, "taker": nil,
		}
		if query.Has("taker") {
			expected.Set("taker", venue.owner)
			expected.Set("swapMode", "ExactIn")
			expected.Set("slippageBps", "1000")
			expected.Set("excludeRouters", "jupiterz,dflow,okx")
			expected.Set("maxSupportedTransactionVersion", "0")
			if !reflect.DeepEqual(query, expected) {
				return venue.reject()
			}
			encoded, err := buildCandidate(venue.owner, jupiter.SwapRequest{
				Side: order.side, AssetMint: fixtureAssetMint, InAmount: rawAmount(order.input),
				MinOutAmount: rawAmount(order.floor), SlippageBPS: 1000,
			}, order.id)
			if err != nil {
				return venue.reject()
			}
			message, err := venueMessage(encoded)
			if err != nil {
				return venue.reject()
			}
			venue.messages[order.id] = message
			response["transaction"] = encoded
			response["taker"] = venue.owner
			response["slippageBps"] = 1000
			response["transactionVersion"] = 0
			response["lastValidBlockHeight"] = fixtureLastValidBlockHeight
			response["gasless"] = false
			response["signatureFeePayer"] = venue.owner
			response["signatureFeeLamports"] = fixtureFee
			response["prioritizationFeeLamports"] = 0
			response["rentFeeLamports"] = 0
		}
		if !reflect.DeepEqual(query, expected) {
			return venue.reject()
		}
		return fixtureResponse(r, response)
	}
	return venue.reject()
}

func (venue *fixtureVenue) execute(r *http.Request) (*http.Response, error) {
	venue.executeCalls++
	var request struct {
		SignedTransaction string `json:"signedTransaction"`
		RequestID         string `json:"requestId"`
		LastValidHeight   string `json:"lastValidBlockHeight"`
	}
	if !decodeVenueRequest(r, &request) || request.LastValidHeight != fixtureLastValidBlockHeight {
		return venue.reject()
	}
	transaction, err := jupiter.ValidateSignedSwap(request.SignedTransaction, venue.owner)
	if err != nil {
		return venue.reject()
	}
	if _, exists := venue.attempts[transaction.Signature]; exists {
		return venue.reject()
	}
	message, err := venueMessage(request.SignedTransaction)
	if err != nil || !bytes.Equal(message, venue.messages[request.RequestID]) {
		return venue.reject()
	}
	for _, order := range venueOrders {
		if request.RequestID == order.id {
			attempt := venueAttempt{order: order, slot: venue.slot + 1}
			venue.attempts[transaction.Signature] = attempt
			if order.side == "BUY" {
				return nil, context.DeadlineExceeded
			}
			return fixtureResponse(r, map[string]any{
				"status": "Success", "signature": transaction.Signature, "code": 0, "slot": rawAmount(attempt.slot),
			})
		}
	}
	return venue.reject()
}

func (venue *fixtureVenue) solanaRPC(r *http.Request) (*http.Response, error) {
	var request struct {
		Version string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}
	if !decodeVenueRequest(r, &request) || request.Version != "2.0" || request.ID != 1 {
		return venue.reject()
	}
	result, ok := venue.rpcResult(request.Method, request.Params)
	if !ok {
		return venue.reject()
	}
	return fixtureResponse(r, map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
}

func (venue *fixtureVenue) rpcResult(method string, params []any) (any, bool) {
	finalized := map[string]any{"commitment": "finalized"}
	confirmed := map[string]any{"commitment": "confirmed"}
	switch method {
	case "getGenesisHash":
		return fixtureGenesis, len(params) == 0
	case "getBalance":
		if !reflect.DeepEqual(params, []any{venue.owner, finalized}) {
			return nil, false
		}
		venue.reads++
		return venue.withSlot(venue.lamports), true
	case "getTokenAccountsByOwner":
		options := map[string]any{"commitment": "finalized", "encoding": "jsonParsed"}
		for _, account := range []struct {
			program, mint string
			address       solana.PublicKey
			amount        uint64
			decimals      uint8
		}{
			{fixtureSPL, fixtureUSDC, solana.PublicKey{4}, venue.usdc, 6},
			{fixtureToken22, fixtureAssetMint, solana.PublicKey{5}, venue.asset, 0},
		} {
			if reflect.DeepEqual(params, []any{venue.owner, map[string]any{"programId": account.program}, options}) {
				record := venue.tokenAccount(account.program, account.mint, account.address, account.amount, account.decimals)
				return venue.withSlot([]any{record}), true
			}
		}
	case "getBlockHeight":
		return uint64(312345600), reflect.DeepEqual(params, []any{confirmed})
	case "isBlockhashValid":
		return venue.withSlot(true), reflect.DeepEqual(params, []any{fixtureBlockhash.String(), confirmed})
	case "getSignatureStatuses":
		return venue.signatureStatus(params)
	case "getTransaction":
		return venue.transaction(params)
	}
	return nil, false
}

func (venue *fixtureVenue) signatureStatus(params []any) (any, bool) {
	if len(params) != 2 || !reflect.DeepEqual(params[1], map[string]any{"searchTransactionHistory": true}) {
		return nil, false
	}
	signatures, ok := params[0].([]any)
	if !ok || len(signatures) != 1 {
		return nil, false
	}
	signature, ok := signatures[0].(string)
	if !ok || signature == "" {
		return nil, false
	}
	var result any
	if attempt := venue.attempts[signature]; attempt.finalized {
		result = map[string]any{"slot": attempt.slot, "confirmationStatus": "finalized", "err": nil}
	}
	return venue.withSlot([]any{result}), true
}

func (venue *fixtureVenue) transaction(params []any) (any, bool) {
	options := map[string]any{"commitment": "finalized", "encoding": "json", "maxSupportedTransactionVersion": json.Number("0")}
	if len(params) != 2 || !reflect.DeepEqual(params[1], options) {
		return nil, false
	}
	signature, ok := params[0].(string)
	if !ok || signature == "" {
		return nil, false
	}
	attempt := venue.attempts[signature]
	if !attempt.finalized {
		return nil, true
	}
	return map[string]any{
		"slot": attempt.slot, "version": 0, "transaction": map[string]any{"signatures": []string{signature}},
		"meta": map[string]any{"err": nil, "fee": fixtureFee},
	}, true
}

func (venue *fixtureVenue) withSlot(value any) any {
	return map[string]any{"context": map[string]uint64{"slot": venue.slot}, "value": value}
}

func (venue *fixtureVenue) tokenAccount(program, mint string, address solana.PublicKey, amount uint64, decimals uint8) any {
	info := map[string]any{
		"owner": venue.owner, "mint": mint, "state": "initialized",
		"tokenAmount": map[string]any{"amount": rawAmount(amount), "decimals": decimals},
	}
	return map[string]any{
		"pubkey": address.String(),
		"account": map[string]any{"owner": program, "executable": false, "data": map[string]any{
			"parsed": map[string]any{"type": "account", "info": info},
		}},
	}
}

func (venue *fixtureVenue) finalize(signature string) error {
	venue.mu.Lock()
	defer venue.mu.Unlock()
	attempt, exists := venue.attempts[signature]
	if !exists || attempt.finalized || attempt.slot != venue.slot+1 || venue.lamports < fixtureFee {
		return errors.New("invalid local finalization")
	}
	order := attempt.order
	switch order.side {
	case "BUY":
		if venue.usdc < order.input {
			return errors.New("insufficient fixture USDC")
		}
		venue.usdc -= order.input
		venue.asset += order.filled
	case "SELL":
		if venue.asset < order.input {
			return errors.New("insufficient fixture asset")
		}
		venue.asset -= order.input
		venue.usdc += order.filled
	}
	venue.lamports -= fixtureFee
	venue.slot = attempt.slot
	attempt.finalized = true
	venue.attempts[signature] = attempt
	return nil
}

func (venue *fixtureVenue) executions() int {
	venue.mu.Lock()
	defer venue.mu.Unlock()
	return venue.executeCalls
}

func (venue *fixtureVenue) balanceReads() int {
	venue.mu.Lock()
	defer venue.mu.Unlock()
	return venue.reads
}

func (venue *fixtureVenue) unexpectedRequests() int {
	venue.mu.Lock()
	defer venue.mu.Unlock()
	return venue.rejected
}

// reject is called only while the venue lock is held.
func (venue *fixtureVenue) reject() (*http.Response, error) {
	venue.rejected++
	return nil, errors.New("request outside the rehearsal fixture")
}

func venueMessage(encoded string) ([]byte, error) {
	transaction, err := solana.TransactionFromBase64(encoded)
	if err != nil {
		return nil, err
	}
	return transaction.Message.MarshalBinary()
}

func decodeVenueRequest(r *http.Request, output any) bool {
	if r.Body == nil {
		return false
	}
	const limit = 8 * 1024
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || len(body) > limit {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	return decoder.Decode(output) == nil && decoder.Decode(new(any)) == io.EOF
}

func fixtureResponse(r *http.Request, value any) (*http.Response, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(body)), Request: r,
	}, nil
}

func rawAmount(amount uint64) string {
	return strconv.FormatUint(amount, 10)
}

func TestVenueTransportRejectsEveryUnexpectedDestination(t *testing.T) {
	venue := newVenue(fixtureAssetMint)
	for _, target := range []string{
		"https://example.invalid/price/v3",
		"https://api.jup.ag.example.invalid/price/v3",
		"http://api.jup.ag/price/v3",
		"https://api.jup.ag:443/price/v3",
		"https://api.jup.ag/another-operation",
		"http://127.0.0.1:2/",
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("x-api-key", fixtureAPIKey)
		if response, err := venue.RoundTrip(request); response != nil || err == nil {
			t.Fatalf("venue accepted unexpected target %s", target)
		}
	}
	if venue.unexpectedRequests() != 6 || venue.executions() != 0 || venue.balanceReads() != 0 {
		t.Fatal("unexpected traffic reached a fixture operation")
	}
}
