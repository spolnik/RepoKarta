package web

import "embed"

// Files contains the production frontend and server-rendered templates.
//
//go:embed all:dist templates/*.html
var Files embed.FS
