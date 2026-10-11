// Package web embeds the dashboard and the Engineering Studio served by the gateway.
package web

import "embed"

// FS contains the operator dashboard (index.html, dashboard.js), the Engineering
// Studio (studio.html, editor.js) and the code and styles they share.
//
//go:embed index.html dashboard.js studio.html editor.js schematic.js schematic.css
var FS embed.FS
