// Package source resolves addons to downloadable releases. GitHub Releases is
// the only host so far; others can implement Source later.
package source

import (
	"context"
	"io"
)

// Latest is the version that means "the newest stable release".
const Latest = "latest"

// Release is one resolved release of an addon.
type Release struct {
	Tag   string
	Asset string // asset file name
	URL   string // asset download URL
}

// Spec says which release and which file of it an addon wants.
type Spec struct {
	// Version is "latest" or an exact tag, matched as an opaque string.
	Version string
	// Asset is an optional glob on asset names. It beats everything else.
	Asset string
	// Flavor is the game flavor, in release.json's names.
	Flavor string
}

// Source is an addon host.
type Source interface {
	// Resolve finds the release of id that spec names and picks its zip.
	Resolve(ctx context.Context, id string, spec Spec) (Release, error)
	// Download writes the asset at url to w.
	Download(ctx context.Context, url string, w io.Writer) error
}
