package contract

import (
	"bytes"
	_ "embed"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed paper-quote-tape.schema.json
var paperQuoteTapeSchema []byte

var PaperQuoteTape, PaperQuoteTapeReceipt = compilePaperQuoteTape()

func compilePaperQuoteTape() (*jsonschema.Schema, *jsonschema.Schema) {
	const root = "https://tradingbot.local/contracts/"
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	for name, body := range map[string][]byte{
		"paper-quote-tape.schema.json": paperQuoteTapeSchema,
		"paper-replay.schema.json":     paperReplaySchema,
		"paper-account.schema.json":    paperSchema,
		"paper-apply.schema.json":      paperApplySchema,
		"portfolio.schema.json":        portfolioSchema,
		"research-job.schema.json":     researchJobSchema,
	} {
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			panic(err)
		}
		if err := compiler.AddResource(root+name, value); err != nil {
			panic(err)
		}
	}
	schema, err := compiler.Compile(root + "paper-quote-tape.schema.json")
	if err != nil {
		panic(err)
	}
	receipt, err := compiler.Compile(root + "paper-quote-tape.schema.json#/$defs/PinReceipt")
	if err != nil {
		panic(err)
	}
	return schema, receipt
}
