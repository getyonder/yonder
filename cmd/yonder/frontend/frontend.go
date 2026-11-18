// Package frontend holds the interface's files for a shell that is not the
// desktop window (the Android spike's, in package mobile). The desktop
// window embeds this folder itself.
package frontend

import "embed"

// Files is the interface: index.html and what it loads.
//
//go:embed index.html app.css app.js shell.css fonts
var Files embed.FS
