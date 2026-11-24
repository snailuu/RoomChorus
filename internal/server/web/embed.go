package web

import "embed"

// Assets holds all embedded static web UI files for host console and listener pages.
//
//go:embed host.html index.html app.css host.js listener.js
var Assets embed.FS
