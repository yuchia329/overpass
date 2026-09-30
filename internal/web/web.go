// Package web holds the Queue page: static HTML and JavaScript, no framework.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

// Queue is the Queue page's file tree, rooted at index.html.
var Queue, _ = fs.Sub(static, "static")
