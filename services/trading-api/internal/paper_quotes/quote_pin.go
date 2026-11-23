package paper_quotes

import (
	"errors"
	"io/fs"

	"tradingbot/trading-api/internal/contract"
)

// QuotePin binds a research job to the exact bytes of one local quote.
type QuotePin struct {
	Version string         `json:"paper_quote_pin_schema_version"`
	JobID   string         `json:"job_id"`
	Quote   QuoteReference `json:"quote"`
}

// QuoteReference locates a quote relative to the local quote directory.
type QuoteReference struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

// DecodeQuotePin checks the pin schema and its binding to the requested job.
func DecodeQuotePin(raw []byte, jobID string) (QuotePin, error) {
	var pin QuotePin
	if contract.Decode(raw, contract.PaperQuotePin, &pin) != nil || pin.JobID != jobID ||
		pin.Quote.Ref == "." || !fs.ValidPath(pin.Quote.Ref) {
		return QuotePin{}, errors.New("invalid local paper quote pin")
	}
	return pin, nil
}
