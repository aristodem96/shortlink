// Package api embeds the OpenAPI description so the running service can serve
// its own documentation, with no risk of the published spec drifting from the
// binary that implements it.
package api

import _ "embed"

// OpenAPISpec is the contents of openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
