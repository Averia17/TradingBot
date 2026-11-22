package jupiter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"
	"unicode/utf8"

	"tradingbot/venue-quotes/internal/quotes"
)

const apiOrigin = "https://api.jup.ag"
const orderPath = "/swap/v2/order"
const maxResponseBytes = 128 * 1024
const keylessInterval = 2 * time.Second

type Client struct {
	apiKey      string
	http        *http.Client
	keylessSlot chan time.Time
}

func New(apiKey string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = maxResponseBytes
	return NewWithTransport(apiKey, transport)
}

// NewWithTransport keeps the Jupiter API origin fixed while allowing a caller to supply its HTTP transport.
func NewWithTransport(apiKey string, transport http.RoundTripper) *Client {
	client := &Client{
		apiKey: apiKey,
		http: &http.Client{
			Transport:     transport,
			Timeout:       5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if apiKey == "" {
		client.keylessSlot = make(chan time.Time, 1)
		client.keylessSlot <- time.Time{}
	}
	return client
}

func (client *Client) CloseIdleConnections() {
	client.http.CloseIdleConnections()
}

type orderResponse struct {
	InputMint   string     `json:"inputMint"`
	OutputMint  string     `json:"outputMint"`
	InAmount    string     `json:"inAmount"`
	OutAmount   string     `json:"outAmount"`
	MinOut      string     `json:"otherAmountThreshold"`
	SwapMode    string     `json:"swapMode"`
	FeeBPS      *uint16    `json:"feeBps"`
	FeeMint     *string    `json:"feeMint"`
	Router      string     `json:"router"`
	RequestID   string     `json:"requestId"`
	ExpiresAt   *time.Time `json:"expireAt"`
	Transaction *string    `json:"transaction"`
	Taker       *string    `json:"taker"`
	ErrorCode   *int       `json:"errorCode"`
	Error       string     `json:"error"`
	ErrorDetail string     `json:"errorMessage"`
	RoutePlan   []struct {
		SwapInfo struct {
			Label string `json:"label"`
		} `json:"swapInfo"`
	} `json:"routePlan"`
}

func (client *Client) Quote(ctx context.Context, request quotes.Request) (quotes.Observation, error) {
	if err := request.Validate(); err != nil {
		return quotes.Observation{}, err
	}
	params := url.Values{
		"inputMint":  {request.InputMint},
		"outputMint": {request.OutputMint},
		"amount":     {request.InAmount},
	}
	var order orderResponse
	requestedAt, observedAt, err := client.fetchJSON(ctx, orderPath, params, &order)
	if err != nil {
		return quotes.Observation{}, err
	}
	return order.observation(request, requestedAt, observedAt)
}

func (client *Client) fetchJSON(ctx context.Context, path string, params url.Values, destination any) (requestedAt, observedAt time.Time, err error) {
	message, err := http.NewRequestWithContext(ctx, http.MethodGet, apiOrigin+path+"?"+params.Encode(), nil)
	if err != nil {
		return time.Time{}, time.Time{}, quotes.ErrUnavailable
	}
	message.Header.Set("Accept", "application/json")
	message.Header.Set("Cache-Control", "no-cache")
	if client.apiKey != "" {
		message.Header.Set("x-api-key", client.apiKey)
	}

	if err := client.waitForKeylessSlot(ctx); err != nil {
		return time.Time{}, time.Time{}, err
	}
	requestedAt = time.Now().UTC()
	response, err := client.http.Do(message)
	if err != nil {
		return requestedAt, time.Time{}, upstreamError(ctx, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return requestedAt, time.Time{}, quotes.ErrRateLimited
	default:
		return requestedAt, time.Time{}, quotes.ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return requestedAt, time.Time{}, upstreamError(ctx, err)
	}
	observedAt = time.Now().UTC()
	if len(raw) > maxResponseBytes || !utf8.Valid(raw) || json.Unmarshal(raw, destination) != nil {
		return requestedAt, observedAt, quotes.ErrUnavailable
	}
	return requestedAt, observedAt, nil
}

func (client *Client) waitForKeylessSlot(ctx context.Context) error {
	if client.keylessSlot == nil {
		return ctx.Err()
	}
	var next time.Time
	select {
	case next = <-client.keylessSlot:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { client.keylessSlot <- next }()
	if delay := time.Until(next); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next = time.Now().Add(keylessInterval)
	return nil
}

func upstreamError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return quotes.ErrUnavailable
}

func (order orderResponse) observation(request quotes.Request, requestedAt, observedAt time.Time) (quotes.Observation, error) {
	if _, valid := order.marketMinimum(request, requestedAt, observedAt); !valid || order.Transaction != nil || order.Taker != nil {
		return quotes.Observation{}, quotes.ErrUnavailable
	}
	var venues []string
	for _, leg := range order.RoutePlan {
		if label := leg.SwapInfo.Label; label != "" && !slices.Contains(venues, label) {
			venues = append(venues, label)
		}
	}
	return quotes.Observation{
		Request: request, Provider: "jupiter", Firmness: "indicative", OutAmount: order.OutAmount,
		MinOutAmount: order.MinOut, SwapFeeBPS: order.FeeBPS, SwapFeeMint: order.FeeMint, Route: order.Router,
		ProviderRequestID: order.RequestID, RequestedAt: requestedAt, ObservedAt: observedAt,
		ProviderExpiresAt: order.ExpiresAt, RouteVenues: venues,
	}, nil
}

func (order orderResponse) marketMinimum(request quotes.Request, requestedAt, observedAt time.Time) (uint64, bool) {
	if order.InputMint != request.InputMint || order.OutputMint != request.OutputMint ||
		order.InAmount != request.InAmount || order.SwapMode != "ExactIn" || order.ErrorCode != nil ||
		order.Error != "" || order.ErrorDetail != "" || order.Router == "" || order.RequestID == "" {
		return 0, false
	}
	out, validOut := quotes.PositiveAmount(order.OutAmount)
	minimum, validMin := quotes.PositiveAmount(order.MinOut)
	return minimum, validOut && validMin && minimum <= out &&
		(order.FeeBPS == nil || *order.FeeBPS <= 10_000) &&
		(order.FeeMint == nil || quotes.ValidMint(*order.FeeMint)) &&
		!observedAt.Before(requestedAt) &&
		(order.ExpiresAt == nil || order.ExpiresAt.After(observedAt))
}
