// Package webassets embeds the production web UI.
package webassets

import (
	"embed"
	"io/fs"
)

//go:generate sh -c "cd ../../web && npm ci && npm run build"
//go:generate go run ./syncdist ../../web/dist dist

// files holds the Vite build. Only dist/.gitkeep is checked in; the bundle
// is generated. all: keeps Vite chunks whose names begin with _ or .
//
//go:embed all:dist
var files embed.FS

// Dist returns the embedded build rooted at its index.html.
func Dist() fs.FS {
	dist, err := fs.Sub(files, "dist")
	if err != nil {
		panic(err)
	}
	return dist
}
