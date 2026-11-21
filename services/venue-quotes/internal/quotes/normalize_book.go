package quotes

import (
	"encoding/json"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Bound the exponent and the mantissa of provider prices and quantities before
// converting them to rationals.
var decimalPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,23})(?:\.([0-9]{1,24}))?(?:[eE]([+-]?[0-9]{1,2}))?$`)

type level struct {
	price *big.Rat
	BookLevel
}

func NormalizeBook(book OrderBook, rawBids, rawAsks [][]json.Number) (OrderBook, error) {
	bids, err := bookSide(rawBids, book.Depth, true)
	if err != nil {
		return OrderBook{}, err
	}
	asks, err := bookSide(rawAsks, book.Depth, false)
	if err != nil {
		return OrderBook{}, err
	}
	if len(bids) == 0 && len(asks) == 0 {
		return OrderBook{}, ErrNoOrderBook
	}
	if len(bids) != 0 && len(asks) != 0 && bids[0].price.Cmp(asks[0].price) > 0 {
		return OrderBook{}, ErrUnavailable
	}
	book.Bids, book.Asks = bookLevels(bids), bookLevels(asks)
	return book, nil
}

func bookSide(values [][]json.Number, depth int, descending bool) ([]level, error) {
	if values == nil || len(values) > MaxBookDepth {
		return nil, ErrUnavailable
	}
	rows := make([]level, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if len(value) != 2 {
			return nil, ErrUnavailable
		}
		price, priceText := positiveDecimal(value[0].String())
		size, sizeText := positiveDecimal(value[1].String())
		if price == nil || size == nil {
			return nil, ErrUnavailable
		}
		if _, duplicate := seen[priceText]; duplicate {
			return nil, ErrUnavailable
		}
		seen[priceText] = struct{}{}
		rows = append(rows, level{price: price, BookLevel: BookLevel{Price: priceText, Quantity: sizeText}})
	}
	slices.SortFunc(rows, func(a, b level) int {
		if descending {
			return b.price.Cmp(a.price)
		}
		return a.price.Cmp(b.price)
	})
	return rows[:min(len(rows), depth)], nil
}

func positiveDecimal(raw string) (*big.Rat, string) {
	parts := decimalPattern.FindStringSubmatch(raw)
	if parts == nil {
		return nil, ""
	}
	exponent, _ := strconv.Atoi(parts[2])
	scale := max(len(parts[1])-exponent, 0)
	if scale > 24 || exponent > 24 {
		return nil, ""
	}
	value, ok := new(big.Rat).SetString(raw)
	if !ok || value.Sign() <= 0 {
		return nil, ""
	}
	text := value.FloatString(scale)
	if scale > 0 {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	return value, text
}

func bookLevels(rows []level) []BookLevel {
	encoded := make([]BookLevel, len(rows))
	for index, row := range rows {
		encoded[index] = row.BookLevel
	}
	return encoded
}
