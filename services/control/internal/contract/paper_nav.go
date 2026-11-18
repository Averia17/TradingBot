package contract

import _ "embed"

//go:embed paper-nav.schema.json
var paperNAVSchema []byte

var PaperNAVRequest = compile(paperNAVSchema, "#/$defs/Request")
