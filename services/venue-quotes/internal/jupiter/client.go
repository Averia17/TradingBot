package jupiter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	"tradingbot/venue-quotes/internal/quotes"
)

const orderURL = "https://api.jup.ag/swap/v2/order"
const maxResponseBytes = 128 * 1024

type Client struct {
	apiKey string
	http   *http.Client
}

func New(apiKey string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = maxResponseBytes
	return &Client{
		apiKey: apiKey,
		http: &http.Client{
			Transport:     transport,
			Timeout:       5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
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
	message, err := http.NewRequestWithContext(ctx, http.MethodGet, orderURL+"?"+params.Encode(), nil)
	if err != nil {
		return quotes.Observation{}, quotes.ErrUnavailable
	}
	message.Header.Set("Accept", "application/json")
	message.Header.Set("Cache-Control", "no-cache")
	if client.apiKey != "" {
		message.Header.Set("x-api-key", client.apiKey)
	}

	requestedAt := time.Now().UTC()
	response, err := client.http.Do(message)
	if err != nil {
		return quotes.Observation{}, upstreamError(ctx, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return quotes.Observation{}, quotes.ErrRateLimited
	default:
		return quotes.Observation{}, quotes.ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return quotes.Observation{}, upstreamError(ctx, err)
	}
	observedAt := time.Now().UTC()
	var order orderResponse
	if len(raw) > maxResponseBytes || !utf8.Valid(raw) || json.Unmarshal(raw, &order) != nil {
		return quotes.Observation{}, quotes.ErrUnavailable
	}
	return order.observation(request, requestedAt, observedAt)
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
	if order.InputMint != request.InputMint || order.OutputMint != request.OutputMint ||
		order.InAmount != request.InAmount || order.SwapMode != "ExactIn" ||
		order.Transaction != nil || order.Taker != nil || order.ErrorCode != nil ||
		order.Error != "" || order.ErrorDetail != "" || order.Router == "" || order.RequestID == "" {
		return quotes.Observation{}, quotes.ErrUnavailable
	}
	out, validOut := quotes.PositiveAmount(order.OutAmount)
	minimum, validMin := quotes.PositiveAmount(order.MinOut)
	if !validOut || !validMin || minimum > out ||
		(order.FeeBPS != nil && *order.FeeBPS > 10_000) ||
		(order.FeeMint != nil && !quotes.ValidMint(*order.FeeMint)) ||
		observedAt.Before(requestedAt) ||
		(order.ExpiresAt != nil && !order.ExpiresAt.After(observedAt)) {
		return quotes.Observation{}, quotes.ErrUnavailable
	}
	return quotes.Observation{
		Request: request, Provider: "jupiter", Firmness: "indicative", OutAmount: order.OutAmount,
		MinOutAmount: order.MinOut, SwapFeeBPS: order.FeeBPS, SwapFeeMint: order.FeeMint, Route: order.Router,
		ProviderRequestID: order.RequestID, RequestedAt: requestedAt, ObservedAt: observedAt,
		ProviderExpiresAt: order.ExpiresAt,
	}, nil
}
