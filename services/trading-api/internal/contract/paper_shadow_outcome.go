package contract

import _ "embed"

//go:embed paper-shadow-outcome.schema.json
var paperShadowOutcomeSchema []byte

var PaperShadowOutcomeRequest = compile(paperShadowOutcomeSchema, "#/$defs/Request")
var PaperShadowObservation = compile(paperShadowOutcomeSchema, "#/$defs/Observation")
var PaperShadowOutcomeReport = compile(paperShadowOutcomeSchema, "#/$defs/Report")
