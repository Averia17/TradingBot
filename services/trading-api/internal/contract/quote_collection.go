package contract

import _ "embed"

//go:embed paper-quote-collection.schema.json
var paperQuoteCollectionSchema []byte

var paperQuoteCollectionResources = []schemaResource{
	{"https://tradingbot.local/paper-account.schema.json", paperSchema},
	{"https://tradingbot.local/paper-replay.schema.json", paperReplaySchema},
}

var PaperQuoteCollectionConfig = compile(paperQuoteCollectionSchema, "#/$defs/Config", paperQuoteCollectionResources...)
var PaperQuoteFixtureResponse = compile(paperQuoteCollectionSchema, "#/$defs/FixtureResponse", paperQuoteCollectionResources...)
