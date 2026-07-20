// Package web embeds the browser client so the server ships as one binary.
package web

import "embed"

//go:embed all:static
var FS embed.FS
