// Package solana_balance reads finalized Solana wallet balances for one owner.
package solana_balance

import (
	"context"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	commitment       = "finalized"
	defaultTTL       = 5 * time.Second
	rpcTimeout       = 5 * time.Second
	maxResponse      = 1 << 20
	base58Digits     = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	tokenProgram     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token2022Program = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
)

var ErrUnavailable = errors.New("Solana wallet balance unavailable")

type Snapshot struct {
	SchemaVersion string                 `json:"schema_version"`
	Network       string                 `json:"network"`
	Owner         string                 `json:"owner"`
	Commitment    string                 `json:"commitment"`
	RequestedAt   time.Time              `json:"requested_at"`
	ObservedAt    time.Time              `json:"observed_at"`
	ExpiresAt     time.Time              `json:"expires_at"`
	Native        NativeSnapshot         `json:"native"`
	TokenPrograms []TokenProgramSnapshot `json:"token_programs"`
}

type NativeSnapshot struct {
	Slot           uint64 `json:"slot"`
	AmountLamports string `json:"amount_lamports"`
}

type TokenProgramSnapshot struct {
	ProgramID string         `json:"program_id"`
	Slot      uint64         `json:"slot"`
	Accounts  []TokenAccount `json:"accounts"`
}

type TokenAccount struct {
	Address   string `json:"address"`
	Mint      string `json:"mint"`
	AmountRaw string `json:"amount_raw"`
	Decimals  uint8  `json:"decimals"`
	State     string `json:"state"`
}

type Client struct {
	rpcURL string
	owner  string
	ttl    time.Duration
	http   *http.Client

	mu     sync.Mutex
	cached *Snapshot
	active *refresh
}

type refresh struct {
	fresh    bool
	waiters  int
	cancel   context.CancelFunc
	done     chan struct{}
	snapshot Snapshot
	err      error
}

func New(rpcURL, owner string, ttl time.Duration) (*Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = 16 << 10
	return NewWithTransport(rpcURL, owner, ttl, transport)
}

// NewWithTransport applies the same endpoint and wallet validation with a supplied HTTP transport.
func NewWithTransport(rpcURL, owner string, ttl time.Duration, transport http.RoundTripper) (*Client, error) {
	if !ValidAddress(owner) {
		return nil, errors.New("invalid Solana wallet address")
	}
	endpoint, err := url.Parse(rpcURL)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" ||
		(endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !loopback(endpoint.Hostname()))) {
		return nil, errors.New("invalid Solana RPC endpoint")
	}
	if ttl < 0 {
		return nil, errors.New("invalid balance cache TTL")
	}
	if ttl == 0 {
		ttl = defaultTTL
	}
	return &Client{
		rpcURL: endpoint.String(), owner: owner, ttl: ttl,
		http: &http.Client{
			Transport:     transport,
			Timeout:       rpcTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (client *Client) CloseIdleConnections() {
	client.http.CloseIdleConnections()
}

// Invalidate clears the cache and detaches any refresh already in progress.
func (client *Client) Invalidate() {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.cached = nil
	client.active = nil
}

// Balances returns a cached snapshot while it is fresh. A fresh read clears the
// cached snapshot and supersedes an ordinary refresh that is already in flight.
func (client *Client) Balances(ctx context.Context, fresh bool) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	client.mu.Lock()
	if fresh {
		client.cached = nil
	}
	if client.cached != nil && time.Now().Before(client.cached.ExpiresAt) {
		snapshot := *client.cached
		client.mu.Unlock()
		return snapshot.clone(), nil
	}
	current := client.active
	if current == nil || (fresh && !current.fresh) {
		// The refresh lives as long as at least one caller still needs it.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rpcTimeout)
		current = &refresh{fresh: fresh, cancel: cancel, done: make(chan struct{})}
		client.active = current
		go client.refresh(readCtx, current)
	}
	current.waiters++
	client.mu.Unlock()

	select {
	case <-current.done:
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		return current.snapshot.clone(), current.err
	case <-ctx.Done():
		client.mu.Lock()
		current.waiters--
		if current.waiters == 0 {
			if client.active == current {
				client.active = nil
			}
			current.cancel()
		}
		client.mu.Unlock()
		return Snapshot{}, ctx.Err()
	}
}

func (client *Client) refresh(ctx context.Context, current *refresh) {
	defer current.cancel()
	snapshot, err := client.read(ctx)
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.active == current {
		client.active = nil
		if err == nil {
			client.cached = &snapshot
		}
	}
	current.snapshot, current.err = snapshot, err
	close(current.done)
}

func (snapshot Snapshot) clone() Snapshot {
	snapshot.RequestedAt = snapshot.RequestedAt.UTC()
	snapshot.ObservedAt = snapshot.ObservedAt.UTC()
	snapshot.ExpiresAt = snapshot.ExpiresAt.UTC()
	snapshot.TokenPrograms = slices.Clone(snapshot.TokenPrograms)
	for i := range snapshot.TokenPrograms {
		snapshot.TokenPrograms[i].Accounts = slices.Clone(snapshot.TokenPrograms[i].Accounts)
	}
	return snapshot
}

func loopback(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

// ValidAddress checks the base58 encoding of a 32-byte Solana public key.
func ValidAddress(address string) bool {
	return validBase58(address, 32, 44)
}

func validBase58(encoded string, decodedLength, maxEncodedLength int) bool {
	if len(encoded) < decodedLength || len(encoded) > maxEncodedLength {
		return false
	}
	var value, digit big.Int
	base := big.NewInt(58)
	for _, character := range encoded {
		index := strings.IndexRune(base58Digits, character)
		if index < 0 {
			return false
		}
		value.Mul(&value, base)
		value.Add(&value, digit.SetInt64(int64(index)))
	}
	leadingZeros := len(encoded) - len(strings.TrimLeft(encoded, "1"))
	return leadingZeros+(value.BitLen()+7)/8 == decodedLength
}
