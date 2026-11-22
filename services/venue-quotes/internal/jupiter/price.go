package jupiter

import (
	"context"
	"encoding/json"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tradingbot/venue-quotes/internal/quotes"
	"tradingbot/venue-quotes/internal/solana_balance"
)

const pricePath = "/price/v3"

// Bound the provider number before allocating its normalized decimal form.
var usdPricePattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,63})(?:\.([0-9]{1,64}))?(?:[eE]([+-]?[0-9]{1,3}))?$`)

type Price struct {
	SchemaVersion string    `json:"schema_version"`
	Network       string    `json:"network"`
	Mint          string    `json:"mint"`
	USDPrice      string    `json:"usd_price"`
	Decimals      uint8     `json:"decimals"`
	BlockID       uint64    `json:"block_id,string"`
	RequestedAt   time.Time `json:"requested_at"`
	ObservedAt    time.Time `json:"observed_at"`
}

type priceEntry struct {
	USDPrice json.RawMessage `json:"usdPrice"`
	Decimals *uint8          `json:"decimals"`
	BlockID  uint64          `json:"blockId"`
}

func (client *Client) Price(ctx context.Context, mint string) (Price, error) {
	if !solana_balance.ValidAddress(mint) {
		return Price{}, ErrInvalidRequest
	}
	var response map[string]priceEntry
	requestedAt, observedAt, err := client.fetchJSON(ctx, pricePath, url.Values{"ids": {mint}}, &response)
	if err != nil {
		return Price{}, err
	}
	entry := response[mint]
	usdPrice := normalizeUSDPrice(string(entry.USDPrice))
	if usdPrice == "" || entry.Decimals == nil || entry.BlockID == 0 || observedAt.Before(requestedAt) {
		return Price{}, quotes.ErrUnavailable
	}
	return Price{
		SchemaVersion: "1", Network: quotes.SolanaMainnet, Mint: mint, USDPrice: usdPrice,
		Decimals: *entry.Decimals, BlockID: entry.BlockID, RequestedAt: requestedAt, ObservedAt: observedAt,
	}, nil
}

func normalizeUSDPrice(raw string) string {
	parts := usdPricePattern.FindStringSubmatch(raw)
	if parts == nil {
		return ""
	}
	exponent, _ := strconv.Atoi(parts[2])
	if exponent < -128 || exponent > 128 {
		return ""
	}
	value, ok := new(big.Rat).SetString(raw)
	if !ok || value.Sign() <= 0 {
		return ""
	}
	scale := len(parts[1]) - exponent
	text := value.FloatString(max(scale, 0))
	if scale > 0 {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	return text
}
