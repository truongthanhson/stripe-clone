// Package openapi embeds the API's OpenAPI document, the source of truth for routes, parameters
// and response shapes.
package openapi

import (
	_ "embed"
)

//go:embed openapi.yaml
var Spec []byte
