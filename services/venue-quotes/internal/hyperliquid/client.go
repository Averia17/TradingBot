// Package hyperliquid captures public trade[XYZ] contract and order book observations.
package hyperliquid

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"tradingbot/venue-quotes/internal/quotes"
)

const (
	infoURL          = "https://api.hyperliquid.xyz/info"
	bookDepth        = 20
	maxResponseBytes = 2 * 1024 * 1024
)

var decimalText = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,15})(\.[0-9]{1,20})?$`)
var positiveDigit = regexp.MustCompile(`[1-9]`)

type Client struct {
	http *http.Client
}

func New() *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = 16 * 1024
	return &Client{http: &http.Client{
		Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }

type RawObservation struct {
	RequestedAt time.Time `json:"requested_at"`
	ReceivedAt  time.Time `json:"received_at"`
	SHA256      string    `json:"sha256"`
	Bytes       []byte    `json:"body_base64"`
}

type Market struct {
	Coin          string  `json:"name"`
	SizeDecimals  *int    `json:"szDecimals"`
	MaxLeverage   int     `json:"maxLeverage"`
	MarginTableID *int    `json:"marginTableId"`
	MarginMode    *string `json:"marginMode"`
	OnlyIsolated  *bool   `json:"onlyIsolated"`
	Delisted      bool    `json:"isDelisted"`
	GrowthMode    *string `json:"growthMode"`
}

type AssetContext struct {
	MarkPrice   string `json:"markPx"`
	OraclePrice string `json:"oraclePx"`
	Funding     string `json:"funding"`
}

type Capture struct {
	Version         string           `json:"schema_version"`
	Provider        string           `json:"provider"`
	Dex             string           `json:"dex"`
	Coin            string           `json:"coin"`
	AvailableAt     time.Time        `json:"available_at"`
	Market          Market           `json:"market"`
	Context         AssetContext     `json:"asset_context"`
	CollateralToken int              `json:"collateral_token_index"`
	OrderBook       quotes.OrderBook `json:"order_book"`
	MetaResponse    RawObservation   `json:"meta_response"`
	BookResponse    RawObservation   `json:"book_response"`
}

type meta struct {
	Universe        []Market `json:"universe"`
	CollateralToken *int     `json:"collateralToken"`
}

type bookLevel struct {
	Price string `json:"px"`
	Size  string `json:"sz"`
}

type bookSnapshot struct {
	Coin   string        `json:"coin"`
	Time   int64         `json:"time"`
	Levels [][]bookLevel `json:"levels"`
}

func (c *Client) Capture(ctx context.Context, symbol string) (Capture, error) {
	request := quotes.BookRequest{Symbol: symbol, Depth: bookDepth}
	if err := request.Validate(); err != nil {
		return Capture{}, err
	}
	coin := "xyz:" + symbol
	metadata, err := c.info(ctx, map[string]string{"type": "metaAndAssetCtxs", "dex": "xyz"})
	if err != nil {
		return Capture{}, fmt.Errorf("hyperliquid metadata: %w", err)
	}
	market, assetContext, collateral, err := parseMeta(metadata.Bytes, coin)
	if err != nil {
		return Capture{}, fmt.Errorf("hyperliquid metadata: %w", err)
	}
	rawBook, err := c.info(ctx, map[string]string{"type": "l2Book", "coin": coin})
	if err != nil {
		return Capture{}, fmt.Errorf("hyperliquid order book: %w", err)
	}
	book, err := parseBook(rawBook, request, coin)
	if err != nil {
		return Capture{}, fmt.Errorf("hyperliquid order book: %w", err)
	}
	return Capture{
		Version: "0.1", Provider: "hyperliquid", Dex: "xyz", Coin: coin, AvailableAt: rawBook.ReceivedAt,
		Market: market, Context: assetContext, CollateralToken: collateral, OrderBook: book,
		MetaResponse: metadata, BookResponse: rawBook,
	}, nil
}

func (c *Client) info(ctx context.Context, query map[string]string) (RawObservation, error) {
	payload, err := json.Marshal(query)
	if err != nil {
		return RawObservation{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, infoURL, bytes.NewReader(payload))
	if err != nil {
		return RawObservation{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	requestedAt := time.Now().UTC()
	response, err := c.http.Do(req)
	if err != nil {
		return RawObservation{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return RawObservation{}, quotes.ErrRateLimited
	default:
		return RawObservation{}, fmt.Errorf("public info endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return RawObservation{}, err
	}
	if len(body) > maxResponseBytes {
		return RawObservation{}, quotes.ErrUnavailable
	}
	return RawObservation{
		RequestedAt: requestedAt, ReceivedAt: time.Now().UTC(),
		SHA256: fmt.Sprintf("%x", sha256.Sum256(body)), Bytes: body,
	}, nil
}

func parseMeta(raw []byte, coin string) (Market, AssetContext, int, error) {
	var parts []json.RawMessage
	var data meta
	var contexts []AssetContext
	if json.Unmarshal(raw, &parts) != nil || len(parts) != 2 ||
		json.Unmarshal(parts[0], &data) != nil || json.Unmarshal(parts[1], &contexts) != nil ||
		len(contexts) != len(data.Universe) || data.CollateralToken == nil || *data.CollateralToken < 0 {
		return Market{}, AssetContext{}, 0, quotes.ErrUnavailable
	}
	idx := -1
	for i, market := range data.Universe {
		if market.Coin == coin {
			if idx != -1 {
				return Market{}, AssetContext{}, 0, quotes.ErrUnavailable
			}
			idx = i
		}
	}
	if idx < 0 || data.Universe[idx].Delisted {
		return Market{}, AssetContext{}, 0, quotes.ErrNoOrderBook
	}
	market, assetContext := data.Universe[idx], contexts[idx]
	if market.SizeDecimals == nil || *market.SizeDecimals < 0 || *market.SizeDecimals > 6 ||
		market.MaxLeverage < 1 || market.MaxLeverage > 100 ||
		!positive(assetContext.MarkPrice) || !positive(assetContext.OraclePrice) ||
		!decimalText.MatchString(assetContext.Funding) {
		return Market{}, AssetContext{}, 0, quotes.ErrUnavailable
	}
	return market, assetContext, *data.CollateralToken, nil
}

func positive(value string) bool {
	return value != "" && value[0] != '-' && decimalText.MatchString(value) && positiveDigit.MatchString(value)
}

func parseBook(raw RawObservation, request quotes.BookRequest, coin string) (quotes.OrderBook, error) {
	var snapshot bookSnapshot
	if json.Unmarshal(raw.Bytes, &snapshot) != nil || snapshot.Coin != coin || snapshot.Time <= 0 ||
		len(snapshot.Levels) != 2 || len(snapshot.Levels[0]) > bookDepth || len(snapshot.Levels[1]) > bookDepth {
		return quotes.OrderBook{}, quotes.ErrUnavailable
	}
	providerTime := time.UnixMilli(snapshot.Time)
	if providerTime.After(raw.ReceivedAt.Add(time.Second)) {
		return quotes.OrderBook{}, errors.New("order book provider timestamp is in the future")
	}
	levels := make([][][]json.Number, 2)
	for i, side := range snapshot.Levels {
		if side == nil {
			return quotes.OrderBook{}, quotes.ErrUnavailable
		}
		levels[i] = make([][]json.Number, 0, len(side))
		for _, row := range side {
			levels[i] = append(levels[i], []json.Number{json.Number(row.Price), json.Number(row.Size)})
		}
	}
	return quotes.NormalizeBook(quotes.OrderBook{
		BookRequest: request, Provider: "hyperliquid", Firmness: "indicative",
		ProviderTimestamp: snapshot.Time, RequestedAt: raw.RequestedAt, ObservedAt: raw.ReceivedAt,
	}, levels[0], levels[1])
}
