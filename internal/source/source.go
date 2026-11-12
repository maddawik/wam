// Package source resolves addons to downloadable releases. GitHub Releases is
// the only host so far; others can implement Source later.
package source

import (
	"context"
	"io"
)

// Release is one resolved release of an addon.
type Release struct {
	Tag   string
	Asset string // asset file name
	URL   string // asset download URL
}

// Source is an addon host.
type Source interface {
	// Resolve finds the release of id that version names. version is "latest"
	// or an exact tag.
	Resolve(ctx context.Context, id, version string) (Release, error)
	// Download writes the asset at url to w.
	Download(ctx context.Context, url string, w io.Writer) error
}
