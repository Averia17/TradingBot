package solana_balance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testOwner   = "83astBRguLMdt2h5U1Tpdq5tjFoJ6noeGwaY3mDLVcri"
	testMint    = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	testAccount = "2PdyH9JcMTZ8818G7cnDdaGPvDS52aHs35PtQcMVsbE7"
	maxAmount   = "18446744073709551615"
)

func TestBalancesPreservesRawUnitsAndIndividualSlots(t *testing.T) {
	var calls atomic.Int32
	client := fixtureClient(t, 0, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeFixture(t, w, r, "")
	})
	snapshot, err := client.Balances(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != "1" || snapshot.Network != "solana-mainnet" || snapshot.Commitment != "finalized" || snapshot.Owner != testOwner {
		t.Fatalf("unexpected snapshot identity %+v", snapshot)
	}
	if snapshot.RequestedAt.IsZero() || snapshot.ObservedAt.Before(snapshot.RequestedAt) || snapshot.ExpiresAt.Sub(snapshot.ObservedAt) != 5*time.Second {
		t.Fatalf("unexpected snapshot timing %+v", snapshot)
	}
	if snapshot.Native.Slot != 11 || snapshot.Native.AmountLamports != maxAmount {
		t.Fatalf("unexpected native balance %+v", snapshot.Native)
	}
	if len(snapshot.TokenPrograms) != 2 || snapshot.TokenPrograms[0].ProgramID != tokenProgram || snapshot.TokenPrograms[0].Slot != 12 ||
		snapshot.TokenPrograms[1].ProgramID != token2022Program || snapshot.TokenPrograms[1].Slot != 13 {
		t.Fatalf("unexpected program snapshots %+v", snapshot.TokenPrograms)
	}
	want := TokenAccount{Address: testAccount, Mint: testMint, AmountRaw: maxAmount, Decimals: 6, State: "frozen"}
	if accounts := snapshot.TokenPrograms[0].Accounts; len(accounts) != 1 || accounts[0] != want {
		t.Fatalf("unexpected token accounts %+v", accounts)
	}
	if accounts := snapshot.TokenPrograms[1].Accounts; accounts == nil || len(accounts) != 0 {
		t.Fatalf("unexpected Token-2022 accounts %+v", accounts)
	}
	snapshot.TokenPrograms[0].Accounts[0].AmountRaw = "0"
	snapshot.TokenPrograms[0].ProgramID = ""
	cached, err := client.Balances(t.Context(), false)
	if err != nil || calls.Load() != 4 || cached.TokenPrograms[0].ProgramID != tokenProgram || cached.TokenPrograms[0].Accounts[0] != want {
		t.Fatalf("cached balance was fetched again or corrupted %+v, %d calls, %v", cached, calls.Load(), err)
	}
}

func TestFreshBalancesDiscardsCachedSnapshotEvenOnFailure(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	client := fixtureClient(t, time.Minute, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeFixture(t, w, r, "")
	})
	if _, err := client.Balances(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if _, err := client.Balances(t.Context(), true); err == nil {
		t.Fatal("fresh read accepted dependency failure")
	}
	if _, err := client.Balances(t.Context(), false); err == nil {
		t.Fatal("ordinary read fell back to invalidated snapshot")
	}
	fail.Store(false)
	if _, err := client.Balances(t.Context(), false); err != nil || calls.Load() != 16 {
		t.Fatalf("refresh did not recover, %d calls, %v", calls.Load(), err)
	}
}

func TestExpiredSnapshotIsRefreshed(t *testing.T) {
	var calls atomic.Int32
	client := fixtureClient(t, 2*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeFixture(t, w, r, "")
	})
	first, err := client.Balances(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	<-time.After(time.Until(first.ExpiresAt))
	second, err := client.Balances(t.Context(), false)
	if err != nil || calls.Load() != 8 || !second.ObservedAt.After(first.ObservedAt) {
		t.Fatalf("expired snapshot was reused, %d calls, %v", calls.Load(), err)
	}
}

func TestSnapshotMatchesPublishedFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../../contracts/valid-jupiter-balances.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if json.Unmarshal(raw, &want) != nil || json.Unmarshal(encoded, &got) != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("balance fixture round trip changed the wire format: %s", encoded)
	}
}

func TestConcurrentBalanceReadsShareRefresh(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	client := fixtureClient(t, time.Minute, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			writeFixture(t, w, r, "")
		case <-r.Context().Done():
		}
	})
	var requests sync.WaitGroup
	for range 8 {
		requests.Go(func() {
			if _, err := client.Balances(t.Context(), false); err != nil {
				t.Errorf("balance read failed: %v", err)
			}
		})
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("balance refresh did not reach the RPC server")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Balances(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller received %v", err)
	}
	unblock()
	requests.Wait()
	if calls.Load() != 4 {
		t.Fatalf("8 reads caused %d RPC calls, want 4", calls.Load())
	}
}

func TestFreshReadSupersedesInFlightOrdinaryRefresh(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
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
	ordinary := make(chan error, 1)
	go func() {
		_, err := client.Balances(t.Context(), false)
		ordinary <- err
	}()
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("ordinary refresh did not reach RPC")
		}
	}
	fresh, err := client.Balances(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-ordinary; err != nil {
		t.Fatal(err)
	}
	cached, err := client.Balances(t.Context(), false)
	if err != nil || calls.Load() != 8 || !cached.ObservedAt.Equal(fresh.ObservedAt) {
		t.Fatalf("ordinary refresh overwrote fresh snapshot, %d calls, %v", calls.Load(), err)
	}
}

func TestBalanceRefreshStopsWhenLastCallerCancels(t *testing.T) {
	started := make(chan struct{}, 4)
	canceled := make(chan struct{}, 4)
	client := fixtureClient(t, 0, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
		canceled <- struct{}{}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.Balances(ctx, false)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not reach RPC")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("balance read returned %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("RPC was still active after the caller canceled")
	}
}

func TestBalancesDoesNotFollowProviderRedirects(t *testing.T) {
	var calls atomic.Int32
	client := fixtureClient(t, 0, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "https://elsewhere.example.invalid", http.StatusTemporaryRedirect)
	})
	if _, err := client.Balances(t.Context(), false); !errors.Is(err, ErrUnavailable) || calls.Load() != 4 {
		t.Fatalf("redirect received %v after %d calls, want four original requests only", err, calls.Load())
	}
}

func TestBalancesRejectsNonMainnetRPC(t *testing.T) {
	client := fixtureClient(t, 0, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
		}
		raw, _ := io.ReadAll(r.Body)
		if json.Unmarshal(raw, &request) == nil && request.Method == "getGenesisHash" {
			_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"}`)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		writeFixture(t, w, r, "")
	})
	if _, err := client.Balances(t.Context(), false); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("non-mainnet RPC was accepted: %v", err)
	}
}

func TestBalancesRejectsMalformedProviderData(t *testing.T) {
	for _, invalid := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":12},"value":null}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"context":{},"value":[]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":12},"value":[{}]}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"context":{"slot":12},"value":[]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":12},"value":[]},"error":{"message":"private-account"}}`,
		strings.Repeat(" ", 2<<20),
	} {
		client := fixtureClient(t, 0, func(w http.ResponseWriter, r *http.Request) { writeFixture(t, w, r, invalid) })
		got, err := client.Balances(t.Context(), false)
		if err == nil || strings.Contains(err.Error(), "private-account") || !got.ObservedAt.IsZero() {
			t.Fatalf("invalid response produced a partial balance or leaked provider detail: %+v %v", got, err)
		}
	}
}

func TestValidAddressRequiresExactly32DecodedBytes(t *testing.T) {
	for _, valid := range []string{testOwner, testMint, tokenProgram, token2022Program, strings.Repeat("1", 32)} {
		if !ValidAddress(valid) {
			t.Errorf("rejected %q", valid)
		}
	}
	for _, invalid := range []string{strings.Repeat("1", 31), strings.Repeat("1", 33), strings.Repeat("z", 44), "0" + testMint[1:]} {
		if ValidAddress(invalid) {
			t.Errorf("accepted %q", invalid)
		}
	}
}

func fixtureClient(t *testing.T, ttl time.Duration, handler http.HandlerFunc) *Client {
	t.Helper()
	client, err := New("https://solana.example.invalid", testOwner, ttl)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Result(), nil
	})
	return client
}

type fixtureTransport func(*http.Request) (*http.Response, error)

func (transport fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func writeFixture(t *testing.T, w http.ResponseWriter, r *http.Request, invalidLegacy string) {
	t.Helper()
	var request struct {
		JSONRPC string            `json:"jsonrpc"`
		ID      int               `json:"id"`
		Method  string            `json:"method"`
		Params  []json.RawMessage `json:"params"`
	}
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&request) != nil || request.JSONRPC != "2.0" || request.ID != 1 {
		t.Errorf("unexpected rpc request %+v", request)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if request.Method != "getGenesisHash" && (len(request.Params) < 2 || string(request.Params[0]) != `"`+testOwner+`"`) {
		t.Errorf("unexpected owner params %s", request.Params)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var result string
	switch request.Method {
	case "getGenesisHash":
		if len(request.Params) != 0 {
			t.Errorf("unexpected genesis hash params %s", request.Params)
		}
		result = `"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"`
	case "getBalance":
		if len(request.Params) != 2 || string(request.Params[1]) != `{"commitment":"finalized"}` {
			t.Errorf("unexpected getBalance params %s", request.Params)
		}
		result = `{"context":{"slot":11},"value":` + maxAmount + `}`
	case "getTokenAccountsByOwner":
		if len(request.Params) != 3 || string(request.Params[2]) != `{"commitment":"finalized","encoding":"jsonParsed"}` {
			t.Errorf("unexpected getTokenAccountsByOwner params %s", request.Params)
		}
		switch string(request.Params[1]) {
		case `{"programId":"` + tokenProgram + `"}`:
			if invalidLegacy != "" {
				_, _ = fmt.Fprint(w, invalidLegacy)
				return
			}
			result = fmt.Sprintf(`{"context":{"slot":12},"value":[{"pubkey":%q,"account":{"owner":%q,"executable":false,"data":{"program":"spl-token","parsed":{"type":"account","info":{"owner":%q,"mint":%q,"state":"frozen","tokenAmount":{"amount":%q,"decimals":6,"uiAmount":18446744073709.55}}}}}}]}`, testAccount, tokenProgram, testOwner, testMint, maxAmount)
		case `{"programId":"` + token2022Program + `"}`:
			result = `{"context":{"slot":13},"value":[]}`
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%s}`, result)
}
