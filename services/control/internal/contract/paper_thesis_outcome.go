package contract

import _ "embed"

//go:embed paper-thesis-outcome.schema.json
var paperThesisOutcomeSchema []byte

var PaperThesisOutcomeRequest = compile(paperThesisOutcomeSchema, "#/$defs/Request")
var PaperThesisOutcomeReport = compile(paperThesisOutcomeSchema, "#/$defs/Report")
