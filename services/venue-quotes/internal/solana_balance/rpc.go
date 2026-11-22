package solana_balance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type rpcOptions struct {
	Commitment string `json:"commitment"`
	Encoding   string `json:"encoding,omitempty"`
}

type rpcProgram struct {
	ProgramID string `json:"programId"`
}

const mainnetGenesisHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"

type rpcTokenAccount struct {
	Pubkey  string `json:"pubkey"`
	Account struct {
		Owner      string `json:"owner"`
		Executable *bool  `json:"executable"`
		Data       struct {
			Parsed struct {
				Type string `json:"type"`
				Info struct {
					Owner  string `json:"owner"`
					Mint   string `json:"mint"`
					State  string `json:"state"`
					Amount struct {
						Raw      string `json:"amount"`
						Decimals *uint8 `json:"decimals"`
					} `json:"tokenAmount"`
				} `json:"info"`
			} `json:"parsed"`
		} `json:"data"`
	} `json:"account"`
}

func (client *Client) read(ctx context.Context) (Snapshot, error) {
	snapshot := Snapshot{
		SchemaVersion: "1", Network: "solana-mainnet", Owner: client.owner, Commitment: commitment,
		RequestedAt: time.Now(), TokenPrograms: make([]TokenProgramSnapshot, 2),
	}
	var calls sync.WaitGroup
	var clusterErr error
	var nativeErr error
	var tokenErrors [2]error
	calls.Go(func() {
		clusterErr = client.checkGenesis(ctx)
	})
	calls.Go(func() {
		slot, amount, err := rpcValue[uint64](ctx, client, "getBalance", client.owner, rpcOptions{Commitment: commitment})
		nativeErr = err
		snapshot.Native = NativeSnapshot{Slot: slot, AmountLamports: strconv.FormatUint(amount, 10)}
	})
	for i, program := range [...]string{tokenProgram, token2022Program} {
		calls.Go(func() {
			snapshot.TokenPrograms[i], tokenErrors[i] = client.readTokens(ctx, program)
		})
	}
	calls.Wait()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := errors.Join(clusterErr, nativeErr, tokenErrors[0], tokenErrors[1]); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Snapshot{}, context.DeadlineExceeded
		}
		return Snapshot{}, ErrUnavailable
	}
	snapshot.ObservedAt = time.Now()
	snapshot.ExpiresAt = snapshot.ObservedAt.Add(client.ttl)
	return snapshot, nil
}

func (client *Client) readTokens(ctx context.Context, program string) (TokenProgramSnapshot, error) {
	slot, records, err := rpcValue[[]rpcTokenAccount](ctx, client, "getTokenAccountsByOwner",
		client.owner, rpcProgram{ProgramID: program}, rpcOptions{Commitment: commitment, Encoding: "jsonParsed"})
	if err != nil {
		return TokenProgramSnapshot{}, err
	}
	accounts := make([]TokenAccount, 0, len(records))
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		parsed := record.Account.Data.Parsed
		info := parsed.Info
		amount, err := strconv.ParseUint(info.Amount.Raw, 10, 64)
		if !ValidAddress(record.Pubkey) || seen[record.Pubkey] || record.Account.Owner != program ||
			record.Account.Executable == nil || *record.Account.Executable || parsed.Type != "account" ||
			info.Owner != client.owner || !ValidAddress(info.Mint) || info.Amount.Decimals == nil ||
			(info.State != "initialized" && info.State != "frozen") || err != nil || strconv.FormatUint(amount, 10) != info.Amount.Raw {
			return TokenProgramSnapshot{}, ErrUnavailable
		}
		seen[record.Pubkey] = true
		accounts = append(accounts, TokenAccount{
			Address: record.Pubkey, Mint: info.Mint, State: info.State,
			AmountRaw: info.Amount.Raw, Decimals: *info.Amount.Decimals,
		})
	}
	slices.SortFunc(accounts, func(a, b TokenAccount) int { return strings.Compare(a.Address, b.Address) })
	return TokenProgramSnapshot{ProgramID: program, Slot: slot, Accounts: accounts}, nil
}

type rpcContextValue[T any] struct {
	Context struct {
		Slot uint64 `json:"slot"`
	} `json:"context"`
	Value *T `json:"value"`
}

func rpcValue[T any](ctx context.Context, client *Client, method string, params ...any) (uint64, T, error) {
	var zero T
	result, err := rpcCall[rpcContextValue[T]](ctx, client, method, params...)
	if err != nil {
		return 0, zero, err
	}
	if result.Context.Slot == 0 || result.Value == nil {
		return 0, zero, ErrUnavailable
	}
	return result.Context.Slot, *result.Value, nil
}

func rpcCall[T any](ctx context.Context, client *Client, method string, params ...any) (T, error) {
	var zero T
	result, err := rpcLookup[T](ctx, client, method, params...)
	if err != nil {
		return zero, err
	}
	if result == nil {
		return zero, ErrUnavailable
	}
	return *result, nil
}

// rpcLookup allows an explicit null response but requires a result member.
func rpcLookup[T any](ctx context.Context, client *Client, method string, params ...any) (*T, error) {
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return nil, ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.rpcURL, bytes.NewReader(body))
	if err != nil {
		return nil, ErrUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-cache")
	response, err := client.http.Do(request)
	if err != nil {
		return nil, rpcError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return nil, rpcError(err)
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Error   json.RawMessage `json:"error"`
		Result  json.RawMessage `json:"result"`
	}
	var result *T
	if len(raw) > maxResponse || !utf8.Valid(raw) || json.Unmarshal(raw, &envelope) != nil ||
		envelope.JSONRPC != "2.0" || envelope.ID != 1 || (len(envelope.Error) > 0 && string(envelope.Error) != "null") ||
		json.Unmarshal(envelope.Result, &result) != nil {
		return nil, ErrUnavailable
	}
	return result, nil
}

func (client *Client) checkGenesis(ctx context.Context) error {
	hash, err := rpcCall[string](ctx, client, "getGenesisHash")
	if err != nil {
		return err
	}
	if hash != mainnetGenesisHash {
		return ErrUnavailable
	}
	return nil
}

func rpcError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return ErrUnavailable
}
