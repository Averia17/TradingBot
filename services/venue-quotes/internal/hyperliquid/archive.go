package hyperliquid

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"tradingbot/venue-quotes/internal/quotes"
)

type MarginTier struct {
	LowerBound  *big.Rat
	MaxLeverage int
}

// Replay rebuilds the observation from the archived provider bytes.
func Replay(raw []byte, symbol string) (Capture, []MarginTier, error) {
	var saved Capture
	request := quotes.BookRequest{Symbol: symbol, Depth: bookDepth}
	coin := "xyz:" + symbol
	if err := request.Validate(); err != nil {
		return Capture{}, nil, err
	}
	if json.Unmarshal(raw, &saved) != nil || saved.Version != "0.1" ||
		saved.Provider != "hyperliquid" || saved.Dex != "xyz" || saved.Coin != coin {
		return Capture{}, nil, fmt.Errorf("invalid trade[XYZ] capture")
	}
	for _, part := range []RawObservation{saved.MetaResponse, saved.BookResponse} {
		if len(part.Bytes) == 0 || len(part.Bytes) > maxResponseBytes || part.RequestedAt.IsZero() ||
			part.ReceivedAt.Before(part.RequestedAt) ||
			part.SHA256 != fmt.Sprintf("%x", sha256.Sum256(part.Bytes)) {
			return Capture{}, nil, fmt.Errorf("invalid archived provider response")
		}
	}
	if saved.BookResponse.RequestedAt.Before(saved.MetaResponse.ReceivedAt) ||
		!saved.AvailableAt.Equal(saved.BookResponse.ReceivedAt) {
		return Capture{}, nil, fmt.Errorf("invalid capture chronology")
	}
	market, context, collateral, err := parseMeta(saved.MetaResponse.Bytes, coin)
	if err != nil {
		return Capture{}, nil, err
	}
	book, err := parseBook(saved.BookResponse, request, coin)
	if err != nil {
		return Capture{}, nil, err
	}
	if time.UnixMilli(book.ProviderTimestamp).After(saved.BookResponse.ReceivedAt) {
		return Capture{}, nil, fmt.Errorf("archived book timestamp follows its receipt")
	}
	tiers, err := marginTiers(saved.MetaResponse.Bytes, market)
	if err != nil {
		return Capture{}, nil, err
	}
	saved.Market, saved.Context, saved.CollateralToken, saved.OrderBook = market, context, collateral, book
	return saved, tiers, nil
}

func marginTiers(raw []byte, market Market) ([]MarginTier, error) {
	if market.MarginTableID == nil || *market.MarginTableID <= 0 {
		return nil, fmt.Errorf("missing margin table identity")
	}
	id := *market.MarginTableID
	if id < 50 {
		if id != market.MaxLeverage {
			return nil, fmt.Errorf("inconsistent single-tier leverage")
		}
		return []MarginTier{{LowerBound: new(big.Rat), MaxLeverage: id}}, nil
	}
	var parts []json.RawMessage
	var meta struct {
		Tables [][]json.RawMessage `json:"marginTables"`
	}
	if json.Unmarshal(raw, &parts) != nil || len(parts) != 2 || json.Unmarshal(parts[0], &meta) != nil {
		return nil, fmt.Errorf("invalid margin table metadata")
	}
	var tiers []MarginTier
	for _, row := range meta.Tables {
		var tableID int
		if len(row) != 2 || json.Unmarshal(row[0], &tableID) != nil {
			return nil, fmt.Errorf("invalid margin table row")
		}
		if tableID != id {
			continue
		}
		if tiers != nil {
			return nil, fmt.Errorf("duplicate margin table")
		}
		var table struct {
			Tiers []struct {
				LowerBound  string `json:"lowerBound"`
				MaxLeverage int    `json:"maxLeverage"`
			} `json:"marginTiers"`
		}
		if json.Unmarshal(row[1], &table) != nil || len(table.Tiers) == 0 {
			return nil, fmt.Errorf("missing margin tiers")
		}
		for i, tier := range table.Tiers {
			if !decimalText.MatchString(tier.LowerBound) || tier.MaxLeverage < 1 ||
				tier.MaxLeverage > market.MaxLeverage {
				return nil, fmt.Errorf("invalid margin tier")
			}
			bound, ok := new(big.Rat).SetString(tier.LowerBound)
			if !ok || i == 0 && bound.Sign() != 0 ||
				i > 0 && (bound.Cmp(tiers[i-1].LowerBound) <= 0 || tier.MaxLeverage > tiers[i-1].MaxLeverage) {
				return nil, fmt.Errorf("unordered margin tiers")
			}
			tiers = append(tiers, MarginTier{LowerBound: bound, MaxLeverage: tier.MaxLeverage})
		}
	}
	if len(tiers) == 0 {
		return nil, fmt.Errorf("unknown margin table %d", id)
	}
	return tiers, nil
}

func (c Capture) OldestMarketTime() time.Time {
	bookTime := time.UnixMilli(c.OrderBook.ProviderTimestamp)
	if bookTime.Before(c.MetaResponse.RequestedAt) {
		return bookTime
	}
	return c.MetaResponse.RequestedAt
}
