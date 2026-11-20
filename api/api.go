// Package api holds the OpenAPI description of the admin API, which the server also serves.
package api

import _ "embed"

// OpenAPI is openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
