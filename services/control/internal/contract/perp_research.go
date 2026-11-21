package contract

import _ "embed"

//go:embed perp-research-study.schema.json
var perpStudySchema []byte

var PerpStudy = compile(perpStudySchema, "")
