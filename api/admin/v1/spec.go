// SPDX-License-Identifier: Apache-2.0

// Package adminv1 embeds the admin API's OpenAPI document.
package adminv1

import _ "embed"

// OpenAPI is api/admin/v1/openapi.yaml, served at /admin/v1/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
