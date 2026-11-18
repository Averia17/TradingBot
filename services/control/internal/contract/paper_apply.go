package contract

import (
	"bytes"
	_ "embed"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed paper-apply.schema.json
var paperApplySchema []byte

//go:embed paper-replay.schema.json
var paperReplaySchema []byte

//go:embed research-job.schema.json
var researchJobSchema []byte

var PaperRiskPolicy, PaperMarks, PaperApplication, PaperFixtureIntent, PaperFixtureQuote, PaperReviewSchedule, PaperQuotePin, PaperEntrySchedule, PaperEntrySelection = compilePaperApplication()

func compilePaperApplication() (policy, marks, request, intent, quote, review, pin, entry, selection *jsonschema.Schema) {
	const root = "https://tradingbot.local/contracts/"
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	for name, body := range map[string][]byte{
		"paper-apply.schema.json":   paperApplySchema,
		"paper-account.schema.json": paperSchema,
		"paper-replay.schema.json":  paperReplaySchema,
		"portfolio.schema.json":     portfolioSchema,
		"research-job.schema.json":  researchJobSchema,
	} {
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			panic(err)
		}
		if err := compiler.AddResource(root+name, value); err != nil {
			panic(err)
		}
	}
	load := func(file, definition string) *jsonschema.Schema {
		schema, err := compiler.Compile(root + file + "#/$defs/" + definition)
		if err != nil {
			panic(err)
		}
		return schema
	}
	const apply, replay = "paper-apply.schema.json", "paper-replay.schema.json"
	return load(apply, "Policy"), load(apply, "Marks"), load(apply, "Request"), load(replay, "Intent"),
		load(replay, "Quote"), load(apply, "ReviewScheduleRequest"), load(apply, "QuotePin"),
		load(apply, "EntryScheduleRequest"), load(apply, "EntrySelection")
}
