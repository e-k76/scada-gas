// Package web embeds the single-page dashboard served by the gateway.
package web

import "embed"

// FS contains index.html.
//
//go:embed index.html
var FS embed.FS
