// Package web embeds the dashboard and the Engineering Studio served by the gateway.
package web

import "embed"

// FS contains index.html (live dashboard), studio.html and editor.js (Engineering Studio).
//
//go:embed index.html studio.html editor.js
var FS embed.FS
