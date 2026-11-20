// Package assets embeds the built web dashboard. The embed path is relative to
// the repo root, which is why this file lives here and cmd/ imports "llm-relay".
package assets

import "embed"

// Web holds web/dist as built by `make web` (see Makefile). The built output
// is committed (see the `!web/dist/` exception in .gitignore) so a fresh
// clone can `go build` without a node toolchain.
//
//go:embed all:web/dist
var Web embed.FS
