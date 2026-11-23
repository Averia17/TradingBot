package contract

import _ "embed"

//go:embed paper-jupiter-fixture.schema.json
var paperJupiterFixtureSchema []byte

var PaperJupiterFixtureReceipt = compile(paperJupiterFixtureSchema, "#/$defs/Receipt")
