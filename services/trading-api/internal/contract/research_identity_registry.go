package contract

import _ "embed"

//go:embed research-identity-registry.schema.json
var researchIdentityRegistrySchema []byte

var ResearchIdentityRegistry = compile(researchIdentityRegistrySchema, "")
