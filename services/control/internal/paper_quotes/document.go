// Package paper_quotes validates synthetic quote tapes and pins, and compares share exposure.
package paper_quotes

import (
	"bytes"
	_ "embed"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"tradingbot/control/internal/contract"
)

const (
	Version       = "0.1"
	Source        = "synthetic_fixture"
	MaxInputBytes = 256 * 1024
)

//go:embed schema.json
var schemaDocument []byte

var validator = compileSchema("")

func compileSchema(fragment string) *jsonschema.Schema {
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaDocument))
	if err != nil {
		panic(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	const location = "https://tradingbot.local/paper-quote-comparison.schema.json"
	if err := c.AddResource(location, value); err != nil {
		panic(err)
	}
	schema, err := c.Compile(location + fragment)
	if err != nil {
		panic(err)
	}
	return schema
}

type document struct {
	Version   string    `json:"schema_version"`
	Source    string    `json:"source_mode"`
	AsOf      time.Time `json:"as_of"`
	Market    market    `json:"market"`
	Registry  registry  `json:"registry"`
	Reference reference `json:"reference"`
	Quotes    []quote   `json:"quotes"`
}

// Token identifies the raw units of a synthetic mint on a single chain.
type Token struct {
	Chain    string `json:"chain"`
	Mint     string `json:"mint"`
	Program  string `json:"program"`
	Decimals int    `json:"decimals"`
}

type mintKey struct{ chain, mint string }

func (t Token) key() mintKey { return mintKey{chain: t.Chain, mint: t.Mint} }

type holding struct {
	Token       Token  `json:"token"`
	QuantityRaw string `json:"quantity_raw"`
}

type market struct {
	UnderlyingID   string   `json:"underlying_id"`
	Chain          string   `json:"chain"`
	Side           string   `json:"side"`
	TargetShares   string   `json:"target_shares"`
	MaxAgeSeconds  int      `json:"max_age_seconds"`
	AllowedIssuers []string `json:"allowed_issuers"`
	Holding        *holding `json:"holding,omitempty"`
}

type registry struct {
	Version  string     `json:"snapshot_version"`
	Wrappers []revision `json:"wrappers"`
}

type revision struct {
	Token          Token     `json:"token"`
	UnderlyingID   string    `json:"underlying_id"`
	IssuerID       string    `json:"issuer_id"`
	Revision       string    `json:"revision"`
	AvailableAt    time.Time `json:"available_at"`
	EffectiveAt    time.Time `json:"effective_at"`
	SharesPerToken ratio     `json:"shares_per_token"`
}

type ratio struct {
	Numerator   string `json:"numerator"`
	Denominator string `json:"denominator"`
}

type reference struct {
	ObservedAt  time.Time      `json:"observed_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
	USDPerShare string         `json:"usd_per_share"`
	Settlements []settlementFX `json:"settlements"`
}

type settlementFX struct {
	Token       Token  `json:"token"`
	USDPerToken string `json:"usd_per_token"`
}

type quote struct {
	ID                string    `json:"id"`
	Mode              string    `json:"mode"`
	Side              string    `json:"side"`
	Wrapper           Token     `json:"wrapper"`
	Revision          string    `json:"registry_revision"`
	Settlement        Token     `json:"settlement"`
	InputRaw          string    `json:"input_raw"`
	ExpectedOutputRaw string    `json:"expected_output_raw"`
	MinOutputRaw      string    `json:"min_output_raw,omitempty"`
	ObservedAt        time.Time `json:"observed_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	Fees              []fee     `json:"fees"`
}

type fee struct {
	ID        string `json:"id"`
	Treatment string `json:"treatment"`
	AmountUSD string `json:"amount_usd"`
}

func parseDocument(data []byte) (document, error) {
	var d document
	if len(data) > MaxInputBytes || !utf8.Valid(data) || contract.Decode(data, validator, &d) != nil {
		return document{}, errors.New("invalid synthetic quote comparison document")
	}
	return d, nil
}
