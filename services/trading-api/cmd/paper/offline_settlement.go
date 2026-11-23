package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"tradingbot/trading-api/internal/accounts"
	"tradingbot/trading-api/internal/contract"
	"tradingbot/trading-api/internal/paper_sim"
	"tradingbot/trading-api/internal/paper_workflow"
)

const jupiterFixtureVersion = "0.1"
const maxFixtureSettlementResponse = 4096

var errFixtureEvidence = errors.New("invalid Jupiter fixture settlement evidence")

type jupiterFixtureRequest struct {
	SchemaVersion string              `json:"schema_version"`
	Source        string              `json:"source_mode"`
	AccountID     string              `json:"account_id"`
	JobID         string              `json:"job_id"`
	OrderID       string              `json:"order_id"`
	QuoteID       string              `json:"quote_id"`
	Instrument    accounts.Instrument `json:"instrument"`
	Side          string              `json:"side"`
	QuantityRaw   string              `json:"quantity_raw"`
	GrossLimitUSD string              `json:"gross_limit_usd"`
	GrossUSD      string              `json:"gross_usd"`
	FeeUSD        string              `json:"fee_usd"`
	CreatedAt     time.Time           `json:"created_at"`
	ExpiresAt     time.Time           `json:"expires_at"`
}

type jupiterFixtureReceipt struct {
	SchemaVersion string `json:"schema_version"`
	Source        string `json:"source_mode"`
	RequestSHA256 string `json:"request_sha256"`
	Status        string `json:"status"`
	QuantityRaw   string `json:"quantity_raw"`
	GrossUSD      string `json:"gross_usd"`
	FeeUSD        string `json:"fee_usd"`
	ObservedAt    string `json:"observed_at"`
}

type jupiterFixtureSettlement struct {
	GrossUSD   string
	ObservedAt time.Time
}

type jupiterFixtureClient struct {
	endpoint string
	http     *http.Client
}

func newJupiterFixtureClient(origin string) (*jupiterFixtureClient, error) {
	invalid := errors.New("--jupiter-fixture-url requires a numeric loopback HTTP origin with an explicit port")
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Opaque != "" ||
		strings.ContainsAny(origin, "?#") || (parsed.EscapedPath() != "" && parsed.EscapedPath() != "/") {
		return nil, invalid
	}
	host := parsed.Hostname()
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || port == 0 || (host != "127.0.0.1" && host != "::1") {
		return nil, invalid
	}
	endpoint := url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.FormatUint(port, 10)), Path: "/v1/paper-fixture/settle"}
	return &jupiterFixtureClient{
		endpoint: endpoint.String(),
		http: &http.Client{
			Transport: &http.Transport{MaxResponseHeaderBytes: 8192, IdleConnTimeout: 30 * time.Second},
			Timeout:   5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Only the opt-in dispatch path decorates Resolve. The underlying store still
// owns the risk-checked reservation and all paper account state.
type jupiterFixtureStore struct {
	accountService
	fixture *jupiterFixtureClient
}

func (store jupiterFixtureStore) JupiterFixtureSettlementEnabled() bool {
	return store.fixture != nil
}

func (store jupiterFixtureStore) Resolve(ctx context.Context, outcome accounts.ResolutionRequest) (accounts.Order, bool, error) {
	if outcome.Outcome != "filled" || outcome.EventID != paper_sim.OutcomeEventID(outcome.OrderID) {
		return accounts.Order{}, false, errors.New("Jupiter fixture dispatch requires a full synthetic fill")
	}
	order, err := store.GetOrder(ctx, outcome.AccountID, outcome.OrderID)
	if err != nil {
		return accounts.Order{}, false, err
	}
	if order.ResearchJobID == "" || (outcome.QuantityRaw != "" && outcome.QuantityRaw != order.QuantityRaw) {
		return accounts.Order{}, false, errFixtureEvidence
	}
	saved, err := priorFixtureFill(order, outcome)
	if err != nil {
		return accounts.Order{}, false, err
	}
	if saved != nil {
		// The first resolution was validated and committed before the application
		// receipt. Replay those exact economics without needing the bridge again.
		outcome.GrossUSD = saved.GrossUSD
		outcome.ObservedAt = saved.ObservedAt
		return store.accountService.Resolve(ctx, outcome)
	}
	evidence, err := store.fixture.settle(ctx, jupiterFixtureRequest{
		SchemaVersion: jupiterFixtureVersion, Source: paper_sim.Source,
		AccountID: order.AccountID, JobID: order.ResearchJobID, OrderID: order.OrderID, QuoteID: order.QuoteID,
		Instrument: order.Instrument, Side: order.Side, QuantityRaw: order.QuantityRaw,
		GrossLimitUSD: canonicalFixtureUSD(order.GrossUSD), GrossUSD: canonicalFixtureUSD(outcome.GrossUSD),
		FeeUSD: canonicalFixtureUSD(outcome.FeeUSD), CreatedAt: order.CreatedAt.UTC(), ExpiresAt: order.ExpiresAt.UTC(),
	})
	if err != nil {
		return accounts.Order{}, false, err
	}
	outcome.GrossUSD = evidence.GrossUSD
	outcome.ObservedAt = evidence.ObservedAt
	return store.accountService.Resolve(ctx, outcome)
}

func priorFixtureFill(order accounts.Order, resolution accounts.ResolutionRequest) (*accounts.Fill, error) {
	switch order.Status {
	case "reserved", "unknown":
		if len(order.Fills) == 0 && order.RemainingQuantityRaw == order.QuantityRaw {
			return nil, nil
		}
	case "filled":
		if len(order.Fills) == 1 {
			fill := order.Fills[0]
			if fill.EventID == resolution.EventID && fill.QuantityRaw == order.QuantityRaw &&
				favorableFixtureGross(order.Side, fill.GrossUSD, resolution.GrossUSD, order.GrossUSD) &&
				sameFixtureAmount(fill.FeeUSD, resolution.FeeUSD) {
				return &fill, nil
			}
		}
	}
	return nil, accounts.ErrConflict
}

func sameFixtureAmount(left, right string) bool {
	if left == right {
		return true
	}
	a, validA := new(big.Rat).SetString(left)
	b, validB := new(big.Rat).SetString(right)
	return validA && validB && a.Cmp(b) == 0
}

func canonicalFixtureUSD(value string) string {
	if strings.Contains(value, ".") {
		return strings.TrimSuffix(strings.TrimRight(value, "0"), ".")
	}
	return value
}

// The amounts are bounded by the ledger or the receipt schema before this
// comparison. Improvement on the expected gross may not cross the reserve.
func favorableFixtureGross(side, gross, planned, limit string) bool {
	actual, validActual := new(big.Rat).SetString(gross)
	expected, validExpected := new(big.Rat).SetString(planned)
	reserved, validReserved := new(big.Rat).SetString(limit)
	if !validActual || !validExpected || !validReserved || actual.Sign() <= 0 {
		return false
	}
	switch side {
	case "BUY":
		return actual.Cmp(expected) <= 0 && actual.Cmp(reserved) <= 0
	case "SELL":
		return actual.Cmp(expected) >= 0 && actual.Cmp(reserved) >= 0
	default:
		return false
	}
}

func (client *jupiterFixtureClient) settle(ctx context.Context, request jupiterFixtureRequest) (jupiterFixtureSettlement, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return jupiterFixtureSettlement{}, errFixtureEvidence
	}
	digest := sha256.Sum256(raw)
	message, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(raw))
	if err != nil {
		return jupiterFixtureSettlement{}, errFixtureEvidence
	}
	message.GetBody = nil
	message.Header.Set("Content-Type", "application/json")
	message.Header.Set("Accept", "application/json")
	response, err := client.http.Do(message)
	if err != nil {
		if ctx.Err() != nil {
			return jupiterFixtureSettlement{}, ctx.Err()
		}
		return jupiterFixtureSettlement{}, errors.New("Jupiter fixture settlement unavailable")
	}
	defer response.Body.Close()
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType != "application/json" || (response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK) {
		return jupiterFixtureSettlement{}, errFixtureEvidence
	}
	raw, err = io.ReadAll(io.LimitReader(response.Body, maxFixtureSettlementResponse+1))
	if err != nil || len(raw) > maxFixtureSettlementResponse || !utf8.Valid(raw) {
		return jupiterFixtureSettlement{}, errFixtureEvidence
	}
	return parseJupiterFixtureSettlement(raw, response.StatusCode, hex.EncodeToString(digest[:]), request)
}

func parseJupiterFixtureSettlement(raw []byte, httpStatus int, digest string, request jupiterFixtureRequest) (jupiterFixtureSettlement, error) {
	var receipt jupiterFixtureReceipt
	if contract.Decode(raw, contract.PaperJupiterFixtureReceipt, &receipt) != nil || receipt.SchemaVersion != jupiterFixtureVersion ||
		receipt.Source != paper_sim.Source || receipt.RequestSHA256 != digest {
		return jupiterFixtureSettlement{}, errFixtureEvidence
	}
	if httpStatus == http.StatusAccepted && receipt.Status == "pending" {
		return jupiterFixtureSettlement{}, paper_workflow.ErrSettlementPending
	}
	if httpStatus != http.StatusOK || receipt.Status != "finalized" || receipt.QuantityRaw != request.QuantityRaw || receipt.FeeUSD != request.FeeUSD ||
		!favorableFixtureGross(request.Side, receipt.GrossUSD, request.GrossUSD, request.GrossLimitUSD) {
		return jupiterFixtureSettlement{}, errFixtureEvidence
	}
	at, err := time.Parse(time.RFC3339Nano, receipt.ObservedAt)
	if err != nil || !strings.HasSuffix(receipt.ObservedAt, "Z") || !at.Equal(at.Truncate(time.Microsecond)) ||
		at.Before(request.CreatedAt) || !at.Before(request.ExpiresAt) {
		return jupiterFixtureSettlement{}, errFixtureEvidence
	}
	return jupiterFixtureSettlement{GrossUSD: canonicalFixtureUSD(receipt.GrossUSD), ObservedAt: at.UTC()}, nil
}
